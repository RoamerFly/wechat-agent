package comfyui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func prepareFixture(t *testing.T) Prepared {
	t.Helper()
	raw := []byte(`{"1":{"class_type":"SaveImage","inputs":{"filename_prefix":"original","images":["2",0]}},"2":{"class_type":"EmptyImage","inputs":{"color":0}}}`)
	ref, err := NewJobRef(digest([]byte(t.Name())), "1")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(raw, digest(raw), ref)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, uint16) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	port := uint16(server.Listener.Addr().(*net.TCPAddr).Port)
	client, err := NewClient(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, port
}

func TestPreparePinsWorkflowAndSaveImage(t *testing.T) {
	p := prepareFixture(t)
	var payload struct {
		ID      string                    `json:"prompt_id"`
		Targets []string                  `json:"partial_execution_targets"`
		Prompt  map[string]map[string]any `json:"prompt"`
	}
	if err := json.Unmarshal(p.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ID != p.Ref().ID || len(payload.Targets) != 1 || payload.Targets[0] != "1" ||
		payload.Prompt["1"]["inputs"].(map[string]any)["filename_prefix"] != p.Ref().subfolder()+"/image" {
		t.Fatalf("wrong submission: %s", p.body)
	}
	for _, raw := range []string{
		`{"nodes":[]}`,
		`{"1":{"class_type":"PreviewImage","inputs":{}}}`,
		`{"1":{"class_type":"SaveImage","class_type":"PreviewImage","inputs":{}}}`,
	} {
		if _, err := Prepare([]byte(raw), digest([]byte(raw)), p.Ref()); err == nil {
			t.Fatalf("unexpected approval for %s", raw)
		}
	}
	if _, err := Prepare([]byte(`{}`), digest([]byte(`{"original":1}`)), p.Ref()); err == nil {
		t.Fatal("changed workflow accepted")
	}
}

func TestUnknownSubmissionLookupAndTargetedCancel(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	prepared := prepareFixture(t)
	ref := prepared.Ref()
	var submits, cancels atomic.Int32
	client, port := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /system_stats":
			fmt.Fprintf(w, `{"system":{"comfyui_version":%q}}`, ReferenceVersion)
		case "POST /prompt":
			submits.Add(1)
			var body struct {
				ID string `json:"prompt_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID != ref.ID {
				t.Errorf("prompt did not use reserved ID: %+v", body)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case "GET /api/jobs/" + ref.ID:
			if cancels.Load() > 0 {
				fmt.Fprintf(w, `{"id":%q,"status":"cancelled"}`, ref.ID)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"status":"in_progress"}`, ref.ID)
		case "POST /api/jobs/" + ref.ID + "/cancel":
			cancels.Add(1)
			fmt.Fprint(w, `{"cancelled":true}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	ctx := context.Background()
	if err := client.Submit(ctx, prepared); !errors.Is(err, ErrSubmissionUnknown) {
		t.Fatalf("submission error = %v", err)
	}
	resumed, err := NewClient(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resumed.CloseIdleConnections)
	reopenedRef, err := NewJobRef(digest([]byte(t.Name())), "1")
	if err != nil || reopenedRef != ref {
		t.Fatalf("reopened reference = %+v, %v", reopenedRef, err)
	}
	if got, err := resumed.Status(ctx, reopenedRef); err != nil || got.State != Running {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if dispatched, err := resumed.Cancel(ctx, ref); !dispatched || err != nil {
		t.Fatalf("cancel = %v, %v", dispatched, err)
	}
	if got, err := resumed.Status(ctx, ref); err != nil || got.State != Cancelled {
		t.Fatalf("cancelled state = %+v, %v", got, err)
	}
	if _, err := resumed.FetchPNG(ctx, ref, 0); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("cancelled image error = %v", err)
	}
	if submits.Load() != 1 || cancels.Load() != 1 {
		t.Fatalf("submit=%d cancel=%d", submits.Load(), cancels.Load())
	}
}

func TestFetchPNGFromCompletedJobOnly(t *testing.T) {
	ref := prepareFixture(t).Ref()
	var good bytes.Buffer
	if err := png.Encode(&good, image.NewNRGBA(image.Rect(0, 0, 5, 7))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		jobID      string
		folder     string
		filename   string
		proof      bool
		data       []byte
		shouldView bool
		success    bool
	}{
		{"valid", ref.ID, ref.subfolder(), "image_00001_.png", true, good.Bytes(), true, true},
		{"wrong job", "056ec5ae-2507-568c-9cb3-aec3f84ec94e", ref.subfolder(), "image_00001_.png", true, good.Bytes(), false, false},
		{"wrong output folder", ref.ID, "another", "image_00001_.png", true, good.Bytes(), false, false},
		{"traversal", ref.ID, ref.subfolder(), "..\\image_00001_.png", true, good.Bytes(), false, false},
		{"wrong extension", ref.ID, ref.subfolder(), "image_00001_Xpng", true, good.Bytes(), false, false},
		{"missing execution proof", ref.ID, ref.subfolder(), "image_00001_.png", false, good.Bytes(), false, false},
		{"truncated", ref.ID, ref.subfolder(), "image_00001_.png", true, good.Bytes()[:good.Len()-10], true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/jobs/" + ref.ID:
					fmt.Fprintf(w, `{"id":%q,"status":"completed","execution_status":{"status_str":"success","completed":%v},"outputs":{"1":{"images":[{"filename":%q,"subfolder":%q,"type":"output"}]},"irrelevant":{"images":true}}}`, tc.jobID, tc.proof, tc.filename, tc.folder)
				case "/view":
					reads.Add(1)
					q := r.URL.Query()
					if q.Get("filename") != tc.filename || q.Get("subfolder") != tc.folder || q.Get("type") != "output" {
						t.Errorf("wrong image query: %v", q)
					}
					_, _ = w.Write(tc.data)
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			})
			got, err := client.FetchPNG(context.Background(), ref, 0)
			if (err == nil) != tc.success || (reads.Load() == 1) != tc.shouldView {
				t.Fatalf("image read=%d, err=%v", reads.Load(), err)
			}
			if tc.success && (got.SHA256 != digest(good.Bytes()) || got.Width != 5 || got.Height != 7 || !bytes.Equal(got.Data, good.Bytes())) {
				t.Fatal("wrong verified image")
			}
		})
	}
}

func TestCompatibilityAndRedirectDoNotSubmit(t *testing.T) {
	p := prepareFixture(t)
	for _, version := range []string{"0.17.0", "redirect"} {
		t.Run(version, func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/system_stats" {
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if version == "redirect" {
					http.Redirect(w, r, "/prompt", http.StatusTemporaryRedirect)
					return
				}
				fmt.Fprintf(w, `{"system":{"comfyui_version":%q}}`, version)
			})
			if err := client.Submit(context.Background(), p); !errors.Is(err, ErrProtocol) {
				t.Fatalf("incompatible service: %v", err)
			}
		})
	}
}

func TestMissingTaskAndCancellationNoOp(t *testing.T) {
	ref := prepareFixture(t).Ref()
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/jobs/" + ref.ID:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"Job not found"}`)
		case "POST /api/jobs/" + ref.ID + "/cancel":
			fmt.Fprint(w, `{"cancelled":false}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	})
	if status, err := client.Status(context.Background(), ref); err != nil || status.State != Unknown {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if dispatched, err := client.Cancel(context.Background(), ref); err != nil || dispatched {
		t.Fatalf("cancel = %v, %v", dispatched, err)
	}
}

// Set AHS_TEST_COMFYUI_PORT only for a dedicated, locally owned CPU fixture.
func TestWindowsPortableComfyUI(t *testing.T) {
	portValue := os.Getenv("AHS_TEST_COMFYUI_PORT")
	if portValue == "" {
		t.Skip("dedicated Windows ComfyUI fixture not configured")
	}
	port, err := strconv.ParseUint(portValue, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(uint16(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	// Salt this fixture's host job so running it twice produces separate outputs.
	ref, err := NewJobRef(digest([]byte(time.Now().UTC().Format(time.RFC3339Nano))), "1")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"1":{"class_type":"SaveImage","inputs":{"images":["2",0]}},"2":{"class_type":"EmptyImage","inputs":{"width":32,"height":24,"batch_size":2,"color":16711680}}}`)
	p, err := Prepare(raw, digest(raw), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Submit(ctx, p); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := client.Status(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == Completed {
			if status.ImageCount != 2 {
				t.Fatalf("saved image count = %d", status.ImageCount)
			}
			break
		}
		if status.State != Pending && status.State != Running {
			t.Fatalf("unexpected job state = %q", status.State)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	for index := 0; index < 2; index++ {
		pngResult, err := client.FetchPNG(ctx, ref, index)
		if err != nil {
			t.Fatal(err)
		}
		result, err := png.Decode(bytes.NewReader(pngResult.Data))
		if err != nil || pngResult.Width != 32 || pngResult.Height != 24 || pngResult.SHA256 != digest(pngResult.Data) {
			t.Fatalf("invalid output %d: %v", index, err)
		}
		r, g, b, _ := result.At(0, 0).RGBA()
		if r != 65535 || g != 0 || b != 0 {
			t.Fatalf("wrong generated color: %d %d %d", r, g, b)
		}
		t.Logf("verified image %d: 32x24 PNG, %d bytes", index, len(pngResult.Data))
	}
	if dispatched, err := client.Cancel(ctx, ref); err != nil || dispatched {
		t.Fatalf("cancel completed job = %v, %v", dispatched, err)
	}
}
