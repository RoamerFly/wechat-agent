package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func shareBindingFixture(t *testing.T, runtimeReady ...bool) (*shareRouteBinding, *pluginhost.Store, pluginhost.Job, pluginhost.Capability) {
	return shareBindingFixtureFor(t, "account", "synthetic-binding", len(runtimeReady) > 0 && runtimeReady[0])
}

func shareBindingFixtureFor(t *testing.T, account, plugin string, runtimeReady bool) (*shareRouteBinding, *pluginhost.Store, pluginhost.Job, pluginhost.Capability) {
	t.Helper()
	ctx := context.Background()
	m := linuxnative.Message{ID: "message", AccountID: account, ThreadID: "room"}
	source := linuxnative.ChannelsSource{AccountID: account, ThreadID: "room", MessageID: "message", ObjectID: "12345"}
	ref := companion.SourceRef{Account: account, Thread: "room", MessageID: "message", EventID: "event", Revision: 2}
	b, err := newShareRouteBinding(m, source, ref, "https://weixin.qq.com/sph/synthetic123")
	if err != nil {
		t.Fatal(err)
	}
	s, err := pluginhost.OpenStore(t.TempDir(), account)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dir := t.TempDir()
	if err = os.CopyFS(dir, os.DirFS("../../plugins/share-reader")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p pluginhost.Package
	json.Unmarshal(raw, &p)
	p.ID = plugin
	_ = runtimeReady // The official package is now staged ready; do not promote a test-only manifest.
	raw, _ = json.Marshal(p)
	if err = os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Prepare(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.Install(ctx, prepared.Token, prepared.Digest, p.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveConfiguration(ctx, p.ID, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{}, Secrets: map[string]string{"yuanbao_cookie": "synthetic=secret"}})
	if err != nil {
		t.Fatal(err)
	}
	i, err = s.SetEnabled(ctx, p.ID, i.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	j := pluginhost.Job{Plugin: p.ID, Version: p.Version, PackageDigest: i.Digest, ConfigRevision: "1", Tool: "share.resolve_channels", Context: pluginhost.Invocation{Account: account, Thread: "room", SourceVersions: map[string]int{"event": 2}}}
	return b, s, j, pluginhost.Capability{ID: j.Tool, Permissions: []string{"network"}}
}

type boundShareTestPolicy struct {
	binding *shareRouteBinding
	store   *pluginhost.Store
	source  linuxnative.ChannelsSource
	calls   int
}

func (p *boundShareTestPolicy) Check(_ context.Context, j pluginhost.Job, _ pluginhost.Capability) error {
	if j.Plugin != "synthetic-binding" || j.Context.Account != "account" || j.Context.Thread != "room" || j.Context.SourceVersions["event"] != 2 {
		return errors.New("synthetic source revoked")
	}
	return nil
}

func (p *boundShareTestPolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	if method != "wechat/http_route" {
		return nil, errors.New("unexpected host service")
	}
	return p.binding.Call(ctx, p.store, j, cap, raw, func(ctx context.Context) error { return p.Check(ctx, j, cap) }, func(_ context.Context, endpoint string, headers map[string]string, _ json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		p.calls++
		if strings.Contains(endpoint, "get_parse_result") {
			if headers["Cookie"] != "synthetic=secret" {
				return nil, errors.New("credential not bound to parse endpoint")
			}
			return json.RawMessage(`{"code":0,"data":{"playable_url":"` + previewURL + `"}}`), nil
		}
		if headers["Cookie"] != "" || headers["Referer"] != previewURL {
			return nil, errors.New("preview credential scope changed")
		}
		return json.RawMessage(verifiedFeed), nil
	})
}

func (p *boundShareTestPolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	verified, source, err := p.binding.ValidateResult(ctx, j, raw, func(ctx context.Context) error { return p.Check(ctx, j, cap) })
	if err == nil {
		p.source = source
	}
	return verified, err
}

func TestLiveShareBindingPythonResultMatchesHostProof(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated Python process; synthetic HTTP only")
	}
	python, err := filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	b, s, job, _ := shareBindingFixture(t, true)
	c, err := pluginruntime.NewController(s, pluginruntime.InstalledExecutor(s, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	i, err := s.Installed(t.Context(), job.Plugin)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(t.Context(), job.Plugin, i.Revision); err != nil {
		t.Fatal(err)
	}
	job.Context.Requester, job.Context.Purpose, job.Context.RequestKey = "synthetic-sender", "read received share", "bound-share"
	req := pluginruntime.CallRequest{Plugin: job.Plugin, Capability: job.Tool, Context: job.Context, Arguments: json.RawMessage(`{"url":"https://weixin.qq.com/sph/synthetic123","expected_object_id":"12345"}`)}
	policy := &boundShareTestPolicy{binding: b, store: s}
	out, err := c.Invoke(t.Context(), req, policy)
	if err != nil || out.Job.State != "completed" || policy.calls != 2 || policy.source.ObjectID != "12345" || policy.source.DecodeKey != "18446744073709551615" {
		t.Fatal("Python/host result contract failed", err, out.Job.State, policy.calls)
	}
	for _, secret := range []string{"synthetic-private", "18446744073709551615", "synthetic=secret"} {
		if strings.Contains(string(out.Job.Result), secret) {
			t.Fatal("private resolver material reached persisted public result")
		}
	}
	second, err := c.Invoke(t.Context(), req, policy)
	if err != nil || !second.Reused || policy.calls != 2 {
		t.Fatal("resolver POST replayed instead of original checkpoint", err)
	}
	private, err := s.PrivateJobResult(t.Context(), second.Job, func(ctx context.Context, j pluginhost.Job) error {
		return policy.Check(ctx, j, pluginhost.Capability{})
	})
	if err != nil {
		t.Fatal("completed resolver lost private recovery", err)
	}
	var recovered linuxnative.ChannelsSource
	if json.Unmarshal(private, &recovered) != nil || recovered != policy.source {
		t.Fatal("recovered media differs from host verified handoff")
	}
}
func routeEnvelope(route, body, referer string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"route": route, "body": json.RawMessage(body), "referer": referer, "max_bytes": 10000, "timeout_seconds": 25})
	return b
}

const parseBody = `{"type":"video_channel_url","url":"https://weixin.qq.com/sph/synthetic123","scene":1}`
const feedBody = `{"baseReq":{"generalToken":"synthetic-token"},"exportId":"export/synthetic"}`
const previewURL = "https://channels.weixin.qq.com/finder-preview/detail?token=synthetic-token&eid=export/synthetic"

func TestShareBindingOnlyOriginalSourceAndObservedChain(t *testing.T) {
	b, s, j, cap := shareBindingFixture(t)
	calls := 0
	check := func(context.Context) error { return nil }
	send := func(_ context.Context, url string, _ map[string]string, _ json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return json.RawMessage(`{"code":0,"data":{"playable_url":"` + previewURL + `"}}`), nil
		}
		return json.RawMessage(`{"errCode":0,"data":{"feedInfo":{"objectId":"12345","videoUrl":"https://media.example/video?token=synthetic-private","decodeKey":"18446744073709551615"}}}`), nil
	}
	for _, bad := range []json.RawMessage{routeEnvelope("channels-feed", feedBody, previewURL), routeEnvelope("channels-parse", `{"type":"video_channel_url","url":"https://weixin.qq.com/sph/different","scene":1}`, ""), routeEnvelope("channels-parse", `{"type":"video_channel_url","url":"https://evil.example","url":"https://weixin.qq.com/sph/synthetic123","scene":1}`, "")} {
		if _, err := b.Call(t.Context(), s, j, cap, bad, check, send); err == nil {
			t.Fatal("foreign/duplicate/out-of-order source accepted")
		}
	}
	if calls != 0 {
		t.Fatal("rejected input reached transport")
	}
	if _, err := b.Call(t.Context(), s, j, cap, routeEnvelope("channels-parse", parseBody, ""), check, send); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(t.Context(), s, j, cap, routeEnvelope("channels-feed", feedBody, previewURL+"other"), check, send); err == nil {
		t.Fatal("different preview token accepted")
	}
	if _, err := b.Call(t.Context(), s, j, cap, routeEnvelope("channels-feed", feedBody, previewURL), check, send); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(t.Context(), s, j, cap, routeEnvelope("channels-feed", feedBody, previewURL), check, send); err == nil {
		t.Fatal("completed POST replayed")
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
func TestShareBindingFailureOrRevocationCannotResumeWithOtherSource(t *testing.T) {
	for _, kind := range []string{"network", "revoked", "response"} {
		t.Run(kind, func(t *testing.T) {
			b, s, j, cap := shareBindingFixture(t)
			calls := 0
			check := func(context.Context) error {
				if kind == "revoked" && calls > 0 {
					return errors.New("revoked")
				}
				return nil
			}
			send := func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
				calls++
				if kind == "network" {
					return nil, errors.New("uncertain")
				}
				return json.RawMessage(`{"code":0,"data":{"playable_url":"https://other.example/finder-preview/?token=x&eid=y"}}`), nil
			}
			req := routeEnvelope("channels-parse", parseBody, "")
			if _, err := b.Call(t.Context(), s, j, cap, req, check, send); err == nil {
				t.Fatal("failure accepted")
			}
			if _, err := b.Call(t.Context(), s, j, cap, req, func(context.Context) error { return nil }, send); err == nil {
				t.Fatal("uncertain request retried")
			}
			if calls != 1 {
				t.Fatal("replayed", calls)
			}
		})
	}
}
func TestShareBindingRejectsScopeRevisionAndFeedObjectConflict(t *testing.T) {
	b, s, j, cap := shareBindingFixture(t)
	for _, alter := range []func(*pluginhost.Job){func(j *pluginhost.Job) { j.Context.Account = "other" }, func(j *pluginhost.Job) { j.Context.Thread = "other" }, func(j *pluginhost.Job) { j.Context.SourceVersions = map[string]int{"event": 1} }} {
		copy := j
		alter(&copy)
		if _, err := b.Call(t.Context(), s, copy, cap, routeEnvelope("channels-parse", parseBody, ""), func(context.Context) error { return nil }, func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
			t.Fatal("scope mismatch reached network")
			return nil, nil
		}); err == nil {
			t.Fatal("scope mismatch accepted")
		}
	}
}

const verifiedFeed = `{"errCode":0,"data":{"authorInfo":{"nickname":"Synthetic author"},"feedInfo":{"objectId":"12345","description":"Synthetic video","videoUrl":"https://media.example/video?token=synthetic-private","decodeKey":"18446744073709551615"}}}`
const verifiedShareResult = `{"ok":true,"platform":"channels","source_url":"https://weixin.qq.com/sph/synthetic123","object_id":"12345","title":"Synthetic video","author":"Synthetic author","private_media_locators":["https://media.example/video?token=synthetic-private"],"private_decode_key":"18446744073709551615","coverage":"media_locator_only","video_observed":false,"media_downloaded":false,"untrusted_external_content":true}`

func completeShareBinding(t *testing.T) (*shareRouteBinding, pluginhost.Job) {
	t.Helper()
	b, s, j, cap := shareBindingFixture(t)
	calls := 0
	send := func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return json.RawMessage(`{"code":0,"data":{"playable_url":"` + previewURL + `"}}`), nil
		}
		return json.RawMessage(verifiedFeed), nil
	}
	for _, req := range []json.RawMessage{routeEnvelope("channels-parse", parseBody, ""), routeEnvelope("channels-feed", feedBody, previewURL)} {
		if _, err := b.Call(t.Context(), s, j, cap, req, func(context.Context) error { return nil }, send); err != nil {
			t.Fatal(err)
		}
	}
	return b, j
}

func TestShareBindingResultPrivateHandoffAndSanitizedCheckpoint(t *testing.T) {
	b, j := completeShareBinding(t)
	check := func(context.Context) error { return nil }
	verified, source, err := b.ValidateResult(t.Context(), j, json.RawMessage(verifiedShareResult), check)
	if err != nil {
		t.Fatal(err)
	}
	if source.AccountID != "account" || source.ThreadID != "room" || source.MessageID != "message" || source.ObjectID != "12345" || source.DecodeKey != "18446744073709551615" || source.MediaURL != "https://media.example/video?token=synthetic-private" {
		t.Fatal("private handoff lost host binding")
	}
	for _, secret := range []string{"synthetic-private", "media.example", "18446744073709551615", "private_media", "private_decode"} {
		if strings.Contains(string(verified.Result), secret) {
			t.Fatal("private handoff leaked into public checkpoint")
		}
	}
	var checkpoint map[string]any
	if json.Unmarshal(verified.Result, &checkpoint) != nil || checkpoint["video_observed"] != false || checkpoint["media_downloaded"] != false || checkpoint["coverage"] != "media_locator_only" {
		t.Fatal("resolver claimed to have observed or downloaded content")
	}
	source.MediaURL = "https://other.example/replaced"
	_, source2, err := b.ValidateResult(t.Context(), j, json.RawMessage(verifiedShareResult), check)
	if err != nil || source2.MediaURL == source.MediaURL {
		t.Fatal("handoff mutation changed stored proof", err)
	}
}

func TestShareBindingRejectsForgedResults(t *testing.T) {
	b, j := completeShareBinding(t)
	mutations := map[string]any{
		"ok": false, "platform": "other", "source_url": "https://weixin.qq.com/sph/different",
		"object_id": "999", "title": "other video", "author": "other person",
		"private_media_locators": []string{"https://other.example/video"}, "private_decode_key": "42",
		"coverage": "full_video", "video_observed": true, "media_downloaded": true,
		"untrusted_external_content": false, "unexpected": "field",
	}
	assertRejected := func(t *testing.T, raw []byte) {
		t.Helper()
		v, source, err := b.ValidateResult(t.Context(), j, raw, func(context.Context) error { return nil })
		if err == nil || len(v.Result) != 0 || source.MediaURL != "" || source.DecodeKey != "" {
			t.Fatal("forged result released a handoff")
		}
	}
	for field, value := range mutations {
		t.Run(field, func(t *testing.T) {
			var data map[string]any
			json.Unmarshal([]byte(verifiedShareResult), &data)
			data[field] = value
			raw, _ := json.Marshal(data)
			assertRejected(t, raw)
		})
	}
	for _, field := range []string{"ok", "video_observed", "media_downloaded", "untrusted_external_content"} {
		t.Run("missing-"+field, func(t *testing.T) {
			var data map[string]any
			json.Unmarshal([]byte(verifiedShareResult), &data)
			delete(data, field)
			raw, _ := json.Marshal(data)
			assertRejected(t, raw)
		})
	}
	assertRejected(t, []byte(strings.Replace(verifiedShareResult, `"ok":true`, `"ok":false,"ok":true`, 1)))
	assertRejected(t, []byte(verifiedShareResult+` {}`))
}

func TestShareBindingResultRechecksScopeAndRevocation(t *testing.T) {
	b, j := completeShareBinding(t)
	for _, alter := range []func(*pluginhost.Job){
		func(j *pluginhost.Job) { j.Context.Account = "other" },
		func(j *pluginhost.Job) { j.Context.Thread = "other" },
		func(j *pluginhost.Job) { j.Context.SourceVersions = map[string]int{"event": 1} },
		func(j *pluginhost.Job) { j.Tool = "web.read" },
	} {
		changed := j
		alter(&changed)
		if _, _, err := b.ValidateResult(t.Context(), changed, json.RawMessage(verifiedShareResult), func(context.Context) error { return nil }); err == nil {
			t.Fatal("result escaped original source scope")
		}
	}
	for _, revokeAt := range []int{1, 2} {
		calls := 0
		_, source, err := b.ValidateResult(t.Context(), j, json.RawMessage(verifiedShareResult), func(context.Context) error {
			calls++
			if calls == revokeAt {
				return errors.New("source forgotten or authorization revoked")
			}
			return nil
		})
		if err == nil || source.MediaURL != "" || calls != revokeAt {
			t.Fatal("revoked result released", calls, err)
		}
	}
	incomplete, _, incompleteJob, _ := shareBindingFixture(t)
	if _, _, err := incomplete.ValidateResult(t.Context(), incompleteJob, json.RawMessage(verifiedShareResult), func(context.Context) error { return nil }); err == nil {
		t.Fatal("incomplete chain released result")
	}
}
