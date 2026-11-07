package devicebroker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wechat-bot/internal/devicespec"
)

func mockIOOptions(t *testing.T) (Options, fixturePaths, *int, *int) {
	t.Helper()
	o, p := fixtureOptions(t)
	writeJSON(t, p.config, validProfile(p.adb))
	o.Legacy = legacyConfig(p.script, p.config, p.storage)
	actions, records := 0, 0
	png := make([]byte, (9<<20)+24)
	copy(png, []byte("\x89PNG\r\n\x1a\n"))
	binary.BigEndian.PutUint32(png[16:20], 100)
	binary.BigEndian.PutUint32(png[20:24], 200)
	video := make([]byte, (4<<20)+32)
	copy(video[4:], []byte("ftyp"))
	o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
		if client != p.adb || len(args) < 7 || strings.Join(args[:6], " ") != "-H 127.0.0.1 -P 5037 -s emulator-5554" {
			t.Fatal("unbound fixed client/serial", client, args)
		}
		v := args[6:]
		joined := strings.Join(v, " ")
		switch {
		case joined == "get-state":
			return []byte("device\n"), nil
		case joined == "shell dumpsys window":
			return []byte("mCurrentFocus=Window{a u0 com.example.browser/.Main}"), nil
		case joined == "exec-out screencap -p":
			return png, nil
		case strings.HasPrefix(joined, "shell uiautomator dump /sdcard/whale-ui-"):
			return []byte("ok"), nil
		case strings.HasPrefix(joined, "exec-out cat /sdcard/whale-ui-"):
			return []byte(`<hierarchy><node text="synthetic" package="com.example.browser" bounds="[0,0][10,10]"/></hierarchy>`), nil
		case strings.HasPrefix(joined, "shell rm -f /sdcard/whale-"):
			return nil, nil
		case strings.HasPrefix(joined, "shell input tap "):
			actions++
			return nil, nil
		case strings.HasPrefix(joined, "shell screenrecord --size 480x800 --bit-rate 256000 --time-limit 2 /sdcard/whale-clip-"):
			records++
			return nil, nil
		case strings.HasPrefix(joined, "exec-out cat /sdcard/whale-clip-"):
			return video, nil
		default:
			t.Fatal("unrecognized native command", v)
			return nil, errors.New("unrecognized mock command")
		}
	}
	return o, p, &actions, &records
}
func ioRequest(t *testing.T, c devicespec.IOCall, step, device string) json.RawMessage {
	t.Helper()
	raw, err := c.Service(t.Context(), step, device, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(step, err)
	}
	return raw
}
func ioObserve(t *testing.T, b devicespec.IOBroker) string {
	t.Helper()
	c, err := b.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "allowed"}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ioRequest(t, c, "profile", "")
	ioRequest(t, c, "foreground", "allowed")
	shot := ioRequest(t, c, "screenshot", "allowed")
	ioRequest(t, c, "ui", "allowed")
	ioRequest(t, c, "foreground", "allowed")
	var ref blobRef
	_ = json.Unmarshal(shot, &ref)
	if ref.Bytes <= 8<<20 {
		t.Fatal("large screenshot silently shortened", ref.Bytes)
	}
	id := ioID("obs-")
	d := b.(*ioBroker).profile.Devices["allowed"]
	s := map[string]any{"id": id, "device_id": "allowed", "package": "com.example.browser", "width": 100, "height": 200, "screenshot_sha256": ref.SHA256, "allowed_packages": d.Packages, "browsers": ioStrings(d.Browsers), "web_hosts": ioStrings(d.WebHosts), "nodes": []any{}, "observed_at": float64(time.Now().UnixNano()) / 1e9}
	result := ioJSON(map[string]any{"snapshot": s, "screenshot_ref": ref})
	bad := bytes.Replace(result, []byte(ref.SHA256), []byte(strings.Repeat("b", 64)), 1)
	if _, err = c.Complete(t.Context(), bad); err == nil {
		t.Fatal("forged screenshot proof accepted")
	}
	raw, err := c.Complete(t.Context(), result)
	if err != nil || !bytes.Contains(raw, []byte("data_base64")) {
		t.Fatal("host raw output reconstruction failed", err)
	}
	return id
}
func TestIOInspectionDoesNotAcquireOrCreatePrivateState(t *testing.T) {
	o, p, _, _ := mockIOOptions(t)
	server := &countingADBServer{}
	o.ADBServer = server
	o.Runner = func(context.Context, string, []string, int64, time.Duration) ([]byte, error) {
		t.Fatal("inspection called the native device runner")
		return nil, errors.New("unexpected device I/O")
	}
	if err := InspectIOOptions(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	o.Actions = []string{"tap"}
	grants, err := InspectIOGrant(t.Context(), o)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grant inspection = %#v, %v", grants, err)
	}
	grant := grants[0]
	if grant.DeviceID != "allowed" || strings.Join(grant.Packages, ",") != "com.example.browser" || strings.Join(grant.Actions, ",") != "observe,tap" {
		t.Fatalf("grant does not reflect the effective host scope: %#v", grant)
	}
	public, err := json.Marshal(grants)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"emulator-5554", p.adb, p.storage, "serial", "web_hosts", "browsers"} {
		if bytes.Contains(public, []byte(private)) {
			t.Fatalf("grant display exposed private connection information: %q", private)
		}
	}
	if server.calls != 0 {
		t.Fatal("readiness started ADB")
	}
	if _, err := os.Stat(filepath.Join(p.state, "devicebroker-private")); !os.IsNotExist(err) {
		t.Fatal("readiness created private state", err)
	}
}

func TestIOInspectionRejectsUnindexedPackageBeforeAcquiringADB(t *testing.T) {
	o, paths, _, _ := mockIOOptions(t)
	server := &countingADBServer{}
	o.ADBServer = server
	if err := os.WriteFile(paths.script, []byte("unindexed synthetic replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenIO(t.Context(), o); !errors.Is(err, ErrNeedsMigration) || server.calls != 0 {
		t.Fatal("changed package acquired ADB authority", err, server.calls)
	}
}

func TestIOProfileFiltersSecretsAliasesAndActionScope(t *testing.T) {
	o, paths, _, _ := mockIOOptions(t)
	p := validProfile(paths.adb)
	d := p.Devices["allowed"]
	d.Actions = append(d.Actions, "home")
	p.Devices["allowed"] = d
	p.Devices["other"] = profileDevice{Serial: "must-not-reach-plugin", Enabled: true, Packages: []string{"com.other.app"}, Actions: []string{"observe"}}
	writeJSON(t, paths.config, p)
	o.Legacy.Command = "never-execute-untrusted-command"
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := b.Begin(t.Context(), "devices", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw := ioRequest(t, c, "profile", "")
	for _, forbidden := range []string{"must-not-reach-plugin", "emulator-5554", "com.other.app", "home", paths.adb, paths.storage, "never-execute"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("unfiltered host profile reached plugin", forbidden)
		}
	}
	if !bytes.Contains(raw, []byte(`"serial":"allowed"`)) || !bytes.Contains(raw, []byte(`"actions":["observe","tap","capture"]`)) {
		t.Fatal("filtered action scope missing", string(raw))
	}
}

func TestIOOptionalUIUnavailableDoesNotInvalidateScreenshotObservation(t *testing.T) {
	o, _, _, _ := mockIOOptions(t)
	base := o.Runner
	o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "uiautomator dump") {
			return nil, errors.New("synthetic hierarchy unavailable")
		}
		return base(ctx, client, args, limit, timeout)
	}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if id := ioObserve(t, b); id == "" {
		t.Fatal("screenshot-only observation lost")
	}
}

func TestIOPublicURLKeepsOriginalIPBoundaryAndExactRequest(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "240.0.0.1", "2001:db8::1", "2002::1", "::ffff:100.64.0.1"} {
		if ioPublicAddress(net.ParseIP(address)) {
			t.Fatal("non-public URL target accepted", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "192.0.0.9", "2606:4700:4700::1111", "2001:1::1"} {
		if !ioPublicAddress(net.ParseIP(address)) {
			t.Fatal("original public target rejected", address)
		}
	}
	o, paths, _, _ := mockIOOptions(t)
	p := validProfile(paths.adb)
	d := p.Devices["allowed"]
	d.Actions = append(d.Actions, "open_url", "text")
	d.WebHosts = []string{"1.1.1.1"}
	p.Devices["allowed"] = d
	writeJSON(t, paths.config, p)
	o.Actions = append(o.Actions, "open_url", "text")
	base := o.Runner
	var original []string
	o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "shell am start") || strings.Contains(strings.Join(args, " "), "shell input text") {
			original = append([]string(nil), args[6:]...)
			return nil, nil
		}
		return base(ctx, client, args, limit, timeout)
	}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	id := ioObserve(t, b)
	for _, request := range []map[string]any{
		{"device_id": "allowed", "action": "open_url", "request_key": "original-url", "observation_id": id, "package": "com.example.browser", "url": "https://1.1.1.1/view?a=1&b=2;literal"},
		{"device_id": "allowed", "action": "text", "request_key": "original-text", "observation_id": id, "text": "Original text 123"},
	} {
		c, err := b.Begin(t.Context(), "act", ioJSON(request))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Service(t.Context(), "dispatch", "allowed", json.RawMessage(`{"url":"https://other.invalid/"}`)); err == nil {
			t.Fatal("plugin substituted original text/URL")
		}
		ioRequest(t, c, "dispatch", "allowed")
		if request["action"] == "open_url" && (len(original) != 9 || original[6] != "'https://1.1.1.1/view?a=1&b=2;literal'") {
			t.Fatal("original URL was not safely bound", original)
		}
		if request["action"] == "text" && strings.Join(original, " ") != "shell input text Original%stext%s123" {
			t.Fatal("original text changed", original)
		}
		_ = c.Close()
	}
}
func TestIOFixedObserveActRecordArtifactAndOldJournalNoReplay(t *testing.T) {
	o, _, actions, records := mockIOOptions(t)
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	id := ioObserve(t, b)
	request := map[string]any{"device_id": "allowed", "action": "tap", "request_key": "original-action", "observation_id": id, "x": 1, "y": 2}
	for attempt := 0; attempt < 2; attempt++ {
		c, err := b.Begin(t.Context(), "act", ioJSON(request))
		if err != nil {
			t.Fatal(err)
		}
		profile := ioRequest(t, c, "profile", "")
		var p map[string]json.RawMessage
		_ = json.Unmarshal(profile, &p)
		var result json.RawMessage
		if attempt == 0 {
			if _, err = c.Service(t.Context(), "dispatch", "other", json.RawMessage(`{}`)); err == nil {
				t.Fatal("other device reached IO")
			}
			if _, err = c.Service(t.Context(), "dispatch", "allowed", json.RawMessage(`{"argv":["shell","id"]}`)); err == nil {
				t.Fatal("free argv reached IO")
			}
			ioRequest(t, c, "dispatch", "allowed")
			result = ioJSON(map[string]any{"state": "submitted", "result": map[string]any{"action": "tap", "device_id": "allowed", "reobserve_required": true}, "replayed": false})
		} else {
			result = p["previous"]
			if len(result) == 0 {
				t.Fatal("old host journal not reused")
			}
		}
		if _, err = c.Complete(t.Context(), result); err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	}
	if *actions != 1 {
		t.Fatal("action replayed", *actions)
	}
	request["x"] = 3
	c, err := b.Begin(t.Context(), "act", ioJSON(request))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Service(t.Context(), "profile", "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("same key changed original request")
	}
	_ = c.Close()
	request = map[string]any{"device_id": "allowed", "action": "capture", "request_key": "original-record", "observation_id": id, "seconds": 2}
	c, err = b.Begin(t.Context(), "act", ioJSON(request))
	if err != nil {
		t.Fatal(err)
	}
	ioRequest(t, c, "profile", "")
	artifact := ioRequest(t, c, "record", "allowed")
	var meta map[string]any
	_ = json.Unmarshal(artifact, &meta)
	if meta["bytes"].(float64) <= 2<<20 {
		t.Fatal("recording silently shortened")
	}
	_, err = c.Complete(t.Context(), ioJSON(map[string]any{"state": "submitted", "result": map[string]any{"action": "capture", "device_id": "allowed", "reobserve_required": true, "artifact": json.RawMessage(artifact)}, "replayed": false}))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c, err = b.Begin(t.Context(), "read_artifact", ioJSON(map[string]any{"device_id": "allowed", "artifact_id": meta["id"], "request_key": "original-record", "offset": 3 << 20, "bytes": 7 << 20}))
	if err != nil {
		t.Fatal(err)
	}
	chunk := ioRequest(t, c, "artifact_read", "allowed")
	if bytes.Contains(chunk, []byte("data_base64")) {
		t.Fatal("binary crossed stdio")
	}
	if _, err = c.Complete(t.Context(), chunk); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if *records != 1 {
		t.Fatal("recording replayed")
	}
}
func TestIOAuthorityRevokedMidCommandPreservesUnknownAndNeverReplays(t *testing.T) {
	o, _, actions, _ := mockIOOptions(t)
	base := o.Runner
	valid := true
	o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
		out, err := base(ctx, client, args, limit, timeout)
		if strings.Contains(strings.Join(args, " "), "shell input tap") {
			valid = false
		}
		return out, err
	}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	id := ioObserve(t, b)
	request := ioJSON(map[string]any{"device_id": "allowed", "action": "tap", "request_key": "unknown-action", "observation_id": id, "x": 1, "y": 2})
	c, err := b.Begin(t.Context(), "act", request)
	if err != nil {
		t.Fatal(err)
	}
	ctx := devicespec.WithIOAuthority(t.Context(), func(context.Context) error {
		if !valid {
			return errors.New("actor revoked")
		}
		return nil
	})
	if _, err = c.Service(ctx, "dispatch", "allowed", json.RawMessage(`{}`)); err == nil {
		t.Fatal("revoked actor obtained success")
	}
	_ = c.Close()
	valid = true
	c, err = b.Begin(t.Context(), "act", request)
	if err != nil {
		t.Fatal(err)
	}
	profile := ioRequest(t, c, "profile", "")
	if !bytes.Contains(profile, []byte(`"state":"unknown"`)) || *actions != 1 {
		t.Fatal("unknown action replayed", string(profile), *actions)
	}
	_ = c.Close()
}

func TestIOCancellationRetainsGlobalLeaseUntilCleanupAndGC(t *testing.T) {
	o, paths, actions, _ := mockIOOptions(t)
	p := validProfile(paths.adb)
	p.Devices["same-alias"] = p.Devices["allowed"]
	p.Devices["different"] = profileDevice{Serial: "emulator-5556", Enabled: true, Packages: []string{"com.example.browser"}, Actions: []string{"observe"}}
	writeJSON(t, paths.config, p)
	otherOptions := o
	otherOptions.Account = "account_2"
	otherOptions.StateDirectory = filepath.Join(filepath.Dir(paths.state), "second-account")
	otherStorage := filepath.Join(otherOptions.StateDirectory, "ledger")
	if err := os.MkdirAll(otherStorage, 0700); err != nil {
		t.Fatal(err)
	}
	otherOptions.Legacy = legacyConfig(paths.script, paths.config, otherStorage)
	otherOptions.Devices = []string{"same-alias", "different"}
	other, err := OpenIO(t.Context(), otherOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); _ = CleanupQuarantined() })
	base := o.Runner
	o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "shell input tap") {
			close(entered)
			<-release // simulate an executor whose tree cleanup is unconfirmed
		}
		return base(ctx, client, args, limit, timeout)
	}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	id := ioObserve(t, b)
	request := ioJSON(map[string]any{"device_id": "allowed", "action": "tap", "request_key": "cancelled-unknown", "observation_id": id, "x": 1, "y": 2})
	c, err := b.Begin(t.Context(), "act", request)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, err := c.Service(t.Context(), "dispatch", "allowed", json.RawMessage(`{}`)); done <- err }()
	<-entered
	if _, err := other.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "same-alias"})); !errors.Is(err, ErrDeviceBusy) {
		t.Fatal("same physical serial admitted across accounts", err)
	}
	different, err := other.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "different"}))
	if err != nil {
		t.Fatal("unrelated serial was blocked", err)
	}
	_ = different.Close()
	if err := b.Close(); err == nil || !strings.Contains(err.Error(), "cleanup_unconfirmed") {
		t.Fatal("unconfirmed cleanup reported success", err)
	}
	if _, err := b.Binding(); err == nil {
		t.Fatal("quarantined broker remained usable")
	}
	b, c = nil, nil
	runtime.GC()
	if _, err := other.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "same-alias"})); !errors.Is(err, ErrDeviceBusy) {
		t.Fatal("GC released unconfirmed physical lease", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err == nil {
		t.Fatal("cancelled native call produced success")
	}
	if err := CleanupQuarantined(); err != nil {
		t.Fatal("confirmed cleanup failed", err)
	}
	free, err := other.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "same-alias"}))
	if err != nil {
		t.Fatal("confirmed cleanup retained lease", err)
	}
	_ = free.Close()
	if *actions != 1 {
		t.Fatal("unknown action was replayed", *actions)
	}
	reopened, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retry, err := reopened.Begin(t.Context(), "act", request)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if raw := ioRequest(t, retry, "profile", ""); !bytes.Contains(raw, []byte(`"state":"unknown"`)) {
		t.Fatal("durable unknown journal was lost", string(raw))
	}
}

func TestIOResourceProofBeforeAndAfterCommands(t *testing.T) {
	for _, resource := range []string{"package", "profile", "adb", "derived"} {
		t.Run(resource, func(t *testing.T) {
			o, paths, _, _ := mockIOOptions(t)
			base := o.Runner
			var b devicespec.IOBroker
			calls := 0
			o.Runner = func(ctx context.Context, client string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
				calls++
				out, err := base(ctx, client, args, limit, timeout)
				path := paths.script
				switch resource {
				case "profile":
					path = paths.config
				case "adb":
					path = paths.adb
				case "derived":
					for _, seal := range b.(*ioBroker).proof.seals {
						if strings.Contains(seal.path, "devicebroker-private") {
							path = seal.path
						}
					}
				}
				if err := os.WriteFile(path, []byte("synthetic changed resource"), 0600); err != nil {
					t.Fatal(err)
				}
				return out, err
			}
			var err error
			b, err = OpenIO(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			first, err := b.Binding()
			second, again := b.Binding()
			if err != nil || again != nil || first == "" || first != second || calls != 0 {
				t.Fatal("read-only binding executed native IO or changed", err, again, calls)
			}
			c, err := b.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "allowed"}))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Service(t.Context(), "foreground", "allowed", json.RawMessage(`{}`)); !errors.Is(err, errChanged) || calls != 1 {
				t.Fatal("post-command changed resource trusted", err, calls)
			}
			if _, err := c.Service(t.Context(), "screenshot", "allowed", json.RawMessage(`{}`)); !errors.Is(err, errChanged) || calls != 1 {
				t.Fatal("pre-command changed resource reached native executor", err, calls)
			}
			if _, err := b.Binding(); !errors.Is(err, errChanged) {
				t.Fatal("tampered resource retained binding", err)
			}
		})
	}
}

func TestIOFailedADBLeaseCleanupIsQuarantinedForRetry(t *testing.T) {
	o, _, _, _ := mockIOOptions(t)
	lease := &trackedADBLease{err: errors.New("synthetic cleanup pending")}
	o.ADBServer = trackedADBServer{lease: lease}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err == nil {
		t.Fatal("unconfirmed ADB cleanup reported success")
	}
	if _, err := b.Binding(); err == nil {
		t.Fatal("unconfirmed cleanup broker remained available")
	}
	lease.err = nil
	if err := CleanupQuarantined(); err != nil || lease.closes != 2 {
		t.Fatal("ADB cleanup was not retried exactly once", err, lease.closes)
	}
	if err := b.Close(); err != nil || lease.closes != 2 {
		t.Fatal("confirmed ADB lease closed again", err, lease.closes)
	}
}

func TestIOUnconfirmedTreeCleanupRetainsLeaseUntilRetry(t *testing.T) {
	o, _, _, _ := mockIOOptions(t)
	cleanupConfirmed := false
	o.Runner = func(context.Context, string, []string, int64, time.Duration) ([]byte, error) {
		return nil, &ioCleanupError{err: errors.New("synthetic native failure"), cleanup: func() error {
			if !cleanupConfirmed {
				return errors.New("device_io_cleanup_unconfirmed")
			}
			return nil
		}}
	}
	b, err := OpenIO(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Begin(t.Context(), "observe", ioJSON(map[string]any{"device_id": "allowed"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Service(t.Context(), "foreground", "allowed", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unconfirmed child failure succeeded")
	}
	if _, err := c.Service(t.Context(), "profile", "", json.RawMessage(`{}`)); err == nil {
		t.Fatal("quarantined call continued IO")
	}
	if err := c.Close(); err == nil {
		t.Fatal("unconfirmed tree cleanup released call")
	}
	key := b.(*ioBroker).leases["allowed"]
	path := filepath.Join(o.GlobalLeaseDirectory, "device-"+key+".lock")
	if lease, err := acquireLease(path); !errors.Is(err, errLeaseBusy) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("unconfirmed tree cleanup released physical lease", err)
	}
	cleanupConfirmed = true
	if err := CleanupQuarantined(); err != nil {
		t.Fatal("confirmed tree cleanup failed", err)
	}
	lease, err := acquireLease(path)
	if err != nil {
		t.Fatal("confirmed cleanup retained physical lock", err)
	}
	_ = lease.Close()
}

func TestIOFixedRunnerProcessBoundary(t *testing.T) {
	mode := os.Args[len(os.Args)-1]
	if strings.HasPrefix(mode, "io-helper-") {
		time.Sleep(50 * time.Millisecond)
		switch mode {
		case "io-helper-environment":
			if os.Getenv("DEVICEBROKER_HOST_SECRET") != "" || os.Getenv("ADB_SERVER_SOCKET") != "" || os.Getenv("HTTP_PROXY") != "" {
				os.Exit(2)
			}
			_, _ = os.Stdout.WriteString("fixed host environment")
		case "io-helper-overflow":
			_, _ = os.Stdout.WriteString(strings.Repeat("x", 4096))
		case "io-helper-block":
			time.Sleep(30 * time.Second)
		}
		os.Exit(0)
	}
	t.Setenv("DEVICEBROKER_HOST_SECRET", "synthetic-must-not-propagate")
	t.Setenv("ADB_SERVER_SOCKET", "synthetic-untrusted")
	t.Setenv("HTTP_PROXY", "synthetic-untrusted")
	args := func(mode string) []string {
		return []string{"-test.run=^TestIOFixedRunnerProcessBoundary$", "--", mode}
	}
	out, err := runFixedADB(t.Context(), os.Args[0], args("io-helper-environment"), 1024, 5*time.Second)
	if err != nil || string(out) != "fixed host environment" {
		t.Fatal("fixed child environment check failed", string(out), err)
	}
	if _, err := runFixedADB(t.Context(), os.Args[0], args("io-helper-overflow"), 100, 5*time.Second); err == nil || !strings.Contains(err.Error(), "response_limit") {
		t.Fatal("explicit resource boundary was silently truncated", err)
	}
	started := time.Now()
	if _, err := runFixedADB(t.Context(), os.Args[0], args("io-helper-block"), 100, 150*time.Millisecond); err == nil || time.Since(started) > 3*time.Second {
		t.Fatal("cancel did not kill fixed child", err, time.Since(started))
	}
}
