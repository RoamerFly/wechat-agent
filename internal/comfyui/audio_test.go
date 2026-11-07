package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func prepareAudioFixture(t *testing.T) Prepared {
	t.Helper()
	raw := []byte(`{"save":{"class_type":"SaveAudioAdvanced","inputs":{"filename_prefix":"original","format":"flac","audio":["music",0]}},"music":{"class_type":"YuE2GenerateMusic","inputs":{}}}`)
	ref, err := NewJobRef(digest([]byte(t.Name())), "save")
	if err != nil {
		t.Fatal(err)
	}
	p, err := PrepareFLAC(raw, digest(raw), ref)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrepareFLACPinsAudioOutput(t *testing.T) {
	p := prepareAudioFixture(t)
	var payload struct {
		Prompt map[string]map[string]any `json:"prompt"`
	}
	if err := json.Unmarshal(p.body, &payload); err != nil {
		t.Fatal(err)
	}
	inputs := payload.Prompt["save"]["inputs"].(map[string]any)
	if got := inputs["filename_prefix"]; got != p.Ref().subfolder()+"/song" {
		t.Fatalf("prefix = %#v", got)
	}
	if got := inputs["format"]; !equalJSON(t, got, "flac") {
		t.Fatalf("format = %#v", got)
	}
	for _, raw := range []string{
		`{"save":{"class_type":"SaveImage","inputs":{"audio":["x",0]}}}`,
		`{"save":{"class_type":"SaveAudioAdvanced","inputs":{}}}`,
	} {
		if _, err := PrepareFLAC([]byte(raw), digest([]byte(raw)), p.Ref()); err == nil {
			t.Fatalf("accepted invalid audio workflow: %s", raw)
		}
	}
}

func TestFetchFLACStrictOutputProof(t *testing.T) {
	ref := prepareAudioFixture(t).Ref()
	valid := []byte("fLaC\x00\x00\x00\x22synthetic")
	for _, tc := range []struct {
		name       string
		jobID      string
		folder     string
		filename   string
		count      int
		proof      bool
		data       []byte
		shouldView bool
		success    bool
	}{
		{"valid", ref.ID, ref.subfolder(), "song_00001.flac", 1, true, valid, true, true},
		{"wrong job", "056ec5ae-2507-568c-9cb3-aec3f84ec94e", ref.subfolder(), "song_00001.flac", 1, true, valid, false, false},
		{"wrong folder", ref.ID, "other", "song_00001.flac", 1, true, valid, false, false},
		{"traversal", ref.ID, ref.subfolder(), "../song_00001.flac", 1, true, valid, false, false},
		{"second result", ref.ID, ref.subfolder(), "song_00001.flac", 2, true, valid, false, false},
		{"not completed", ref.ID, ref.subfolder(), "song_00001.flac", 1, false, valid, false, false},
		{"not flac", ref.ID, ref.subfolder(), "song_00001.flac", 1, true, []byte("RIFFfake"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var views atomic.Int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/jobs/" + ref.ID:
					audios := ""
					for i := 0; i < tc.count; i++ {
						if i > 0 {
							audios += ","
						}
						audios += fmt.Sprintf(`{"filename":%q,"subfolder":%q,"type":"output"}`, tc.filename, tc.folder)
					}
					fmt.Fprintf(w, `{"id":%q,"status":"completed","execution_status":{"status_str":"success","completed":%v},"outputs":{"save":{"audio":[%s]}}}`, tc.jobID, tc.proof, audios)
				case "/view":
					views.Add(1)
					if q := r.URL.Query(); q.Get("filename") != tc.filename || q.Get("subfolder") != tc.folder || q.Get("type") != "output" {
						t.Errorf("wrong view query %v", q)
					}
					_, _ = w.Write(tc.data)
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			})
			got, err := client.FetchFLAC(context.Background(), ref)
			if (err == nil) != tc.success || (views.Load() == 1) != tc.shouldView {
				t.Fatalf("view=%d err=%v", views.Load(), err)
			}
			if tc.success && (got.Filename != "song_00001.flac" || got.SHA256 != digest(valid) || !bytes.Equal(got.Data, valid)) {
				t.Fatalf("unexpected FLAC %#v", got)
			}
		})
	}
}

func TestUploadWAVUsesManagedFlatName(t *testing.T) {
	ref := prepareAudioFixture(t).Ref()
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0}, 32)...)
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload/image" || r.Method != http.MethodPost {
			t.Errorf("unexpected upload %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content type = %q, %v", r.Header.Get("Content-Type"), err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string][]byte{}
		filename := ""
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			data := new(bytes.Buffer)
			_, _ = data.ReadFrom(part)
			fields[part.FormName()] = data.Bytes()
			if part.FormName() == "image" {
				filename = part.FileName()
			}
		}
		expected := "station-" + ref.ID + ".wav"
		if filename != expected || string(fields["type"]) != "input" || !bytes.Equal(fields["image"], wav) {
			t.Errorf("unexpected multipart filename=%q fields=%q", filename, fields)
		}
		fmt.Fprintf(w, `{"name":%q,"subfolder":"","type":"input"}`, expected)
	})
	got, err := client.UploadWAV(context.Background(), ref, wav)
	if err != nil || got != "station-"+ref.ID+".wav" {
		t.Fatalf("upload = %q, %v", got, err)
	}
	if _, err := client.UploadWAV(context.Background(), ref, []byte("not wav")); err == nil {
		t.Fatal("non-WAV upload accepted")
	}
}

func TestYuERequiredNodeSchemasAreNarrow(t *testing.T) {
	allowed := map[string]bool{"YuE2GenerateMusic": true, "YuE2GenerateABC": true, "KSampler": true}
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/object_info/"):]
		if !allowed[name] {
			t.Errorf("unallowlisted schema request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"%s":{"input":{"required":{"x":["STRING",{}]}},"display_name":%q}}`, name, name)
	})
	raw, err := client.YuERequiredNodeSchemas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if json.Unmarshal(raw, &got) != nil || len(got) != len(allowed) {
		t.Fatalf("schemas=%s", raw)
	}
	for name := range allowed {
		if len(got[name]) == 0 {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestOwnedClientRechecksListenerOnEveryNewDial(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/system_stats" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"system":{"comfyui_version":%q}}`, ReferenceVersion)
	}))
	defer server.Close()
	port := uint16(server.Listener.Addr().(*net.TCPAddr).Port)
	client, err := NewOwnedClient(port, func() error {
		checks.Add(1)
		if !allowed.Load() {
			return fmt.Errorf("owned process is gone")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if err := client.Check(context.Background()); err != nil || checks.Load() < 3 {
		t.Fatalf("first owned check err=%v verifies=%d", err, checks.Load())
	}
	client.CloseIdleConnections() // force a new TCP dial rather than reuse.
	allowed.Store(false)
	if err := client.Check(context.Background()); err == nil {
		t.Fatal("client dialed after listener ownership was revoked")
	}
}

func equalJSON(t *testing.T, got, want any) bool {
	t.Helper()
	a, errA := json.Marshal(got)
	b, errB := json.Marshal(want)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}
