package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func boundVisualInvocation() pluginhost.Invocation {
	return pluginhost.Invocation{Account: "test", Thread: "host:synthetic-visual-origin", Requester: "administrator", Purpose: "observe verified synthetic image source", RequestKey: "synthetic-bound-visual", Authorization: "synthetic-origin-proof", SourceVersions: map[string]int{"synthetic-image-source": 2}}
}

func boundVisualFixture(t *testing.T) (*webFixture, *mediaPluginPolicy, *bool) {
	t.Helper()
	f, _, incoming, _ := mediaPolicyFixture(t, false)
	valid := true
	policy, err := f.manager.newBoundMediaVisualObservationPolicy(t.Context(), incoming.Images(), boundVisualInvocation(), 2, func(ctx context.Context) error {
		if !valid {
			return errors.New("synthetic_visual_source_revoked")
		}
		return ctx.Err()
	}, "Describe only these verified source images", 2048)
	if err != nil {
		t.Fatal(err)
	}
	return f, policy, &valid
}

func verifiedVisualPluginReply(t *testing.T, policy *mediaPluginPolicy, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ok": true, "observation": text, "status": "observed", "sources": policy.Parameters().Refs, "coverage": "provided_images_only", "untrusted_external_content": true, "audio_observed": false, "requested_model": policy.Parameters().Model, "requested_max_tokens": policy.Parameters().Tokens, "thinking": policy.Parameters().Thinking, "reasoning_effort": nil, "provider_model": nil, "usage": nil})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func completeBoundVisualForTest(t *testing.T, policy *mediaPluginPolicy) pluginhost.Job {
	t.Helper()
	job, capability := mediaJob(policy)
	job.Arguments = policy.Arguments()
	job, created, err := policy.store.CreateJob(t.Context(), job)
	if err != nil || !created {
		t.Fatal("visual intent was not created", err)
	}
	job, err = policy.store.ClaimJob(t.Context(), job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		return json.RawMessage(mediaReply), nil
	}
	if _, err := policy.Service(t.Context(), job, capability, "wechat/model_request", mediaRequest(policy)); err != nil {
		t.Fatal(err)
	}
	verified, err := policy.Validate(t.Context(), job, capability, verifiedVisualPluginReply(t, policy, "Synthetic pixels"))
	if err != nil {
		t.Fatal(err)
	}
	job, err = policy.store.CompleteJobWithPrivate(t.Context(), job.ID, job.Revision, nil, verified.Result, verified.Private)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Check(t.Context(), job, pluginhost.Capability{}); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestBoundMediaVisualUsesPluginConfigurationAndHostSourceWithoutNativeMessage(t *testing.T) {
	f, policy, _ := boundVisualFixture(t)
	if policy.receivedSharePolicy != nil || policy.Invocation().Thread != "host:synthetic-visual-origin" || len(policy.Invocation().SourceVersions) != 1 || policy.Parameters().Refs[0].Version != 2 || policy.Parameters().Tokens != 2048 {
		t.Fatal("bound visual origin fabricated an incoming message or lost source/version")
	}
	f.manager.config.Enabled = false
	f.manager.config.Behavior.ReadImages = false
	f.manager.config.Model, f.manager.config.APIKey = "legacy-model-must-not-be-used", "legacy-key-must-not-be-used"
	job, capability := mediaJob(policy)
	calls := 0
	policy.post = func(_ context.Context, endpoint string, headers map[string]string, body json.RawMessage, _ int64, _ time.Duration) (json.RawMessage, error) {
		calls++
		if endpoint != "https://provider.example/chat/completions" || headers["Authorization"] != "Bearer synthetic-private-key" || !bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString(policy.Images()[0].Data))) || bytes.Contains(body, []byte("legacy-")) || bytes.Contains(body, []byte("$media_ref")) {
			t.Fatal("bound visual route did not use plugin-owned configuration/source")
		}
		return json.RawMessage(mediaReply), nil
	}
	if _, err := policy.Validate(t.Context(), job, capability, verifiedVisualPluginReply(t, policy, "Synthetic pixels")); err == nil {
		t.Fatal("plugin claim without actual host provider response was trusted")
	}
	if _, err := policy.Service(t.Context(), job, capability, "wechat/model_request", mediaRequest(policy)); err != nil || calls != 1 {
		t.Fatal("bound visual service failed", calls, err)
	}
	verified, err := policy.Validate(t.Context(), job, capability, verifiedVisualPluginReply(t, policy, "Synthetic pixels"))
	if err != nil || !bytes.Contains(verified.Private, []byte("Synthetic pixels")) || bytes.Contains(verified.Result, []byte("Synthetic pixels")) {
		t.Fatal("visual observation private/public split failed", err)
	}
	if _, err := policy.Validate(t.Context(), job, capability, verifiedVisualPluginReply(t, policy, "fabricated observation")); err == nil {
		t.Fatal("forged observation replaced actual provider result")
	}
}

func TestBoundMediaVisualRejectsRevocationDuringModelAndAfterCompletion(t *testing.T) {
	t.Run("during_model", func(t *testing.T) {
		_, policy, valid := boundVisualFixture(t)
		job, capability := mediaJob(policy)
		calls := 0
		policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
			calls++
			*valid = false
			return json.RawMessage(mediaReply), nil
		}
		if _, err := policy.Service(t.Context(), job, capability, "wechat/model_request", mediaRequest(policy)); err == nil {
			t.Fatal("revoked model response was retained", err)
		}
		if _, err := policy.Service(t.Context(), job, capability, "wechat/model_request", mediaRequest(policy)); err == nil || calls != 1 {
			t.Fatal("revoked model request replayed", err, calls)
		}
		*valid = true
		if _, err := policy.Validate(t.Context(), job, capability, verifiedVisualPluginReply(t, policy, "Synthetic pixels")); err == nil {
			t.Fatal("revoked model response was retained after authority was restored")
		}
	})
	t.Run("after_completion", func(t *testing.T) {
		_, policy, valid := boundVisualFixture(t)
		completed := completeBoundVisualForTest(t, policy)
		*valid = false
		if err := policy.Check(t.Context(), completed, pluginhost.Capability{}); err == nil {
			t.Fatal("completed visual result ignored source revocation")
		}
		if _, err := policy.store.PrivateJobResult(t.Context(), completed, func(ctx context.Context, job pluginhost.Job) error {
			return policy.Check(ctx, job, pluginhost.Capability{})
		}); err == nil {
			t.Fatal("revoked private visual observation remained readable")
		}
	})
}

func TestBoundMediaVisualCopiesSourceInputsAndRequiresAuthority(t *testing.T) {
	f, _, incoming, _ := mediaPolicyFixture(t, false)
	invocation := boundVisualInvocation()
	images := []deepseek.Image{{Data: append([]byte(nil), incoming.Images()[0].Data...)}}
	policy, err := f.manager.newBoundMediaVisualObservationPolicy(t.Context(), images, invocation, 2, func(context.Context) error { return nil }, "copy source", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), policy.Images()[0].Data...)
	images[0].Data[0] ^= 1
	invocation.SourceVersions["synthetic-image-source"] = 99
	if !bytes.Equal(before, policy.Images()[0].Data) || policy.Invocation().SourceVersions["synthetic-image-source"] != 2 {
		t.Fatal("caller mutated an approved visual binding")
	}
	returned := policy.Invocation()
	returned.SourceVersions["synthetic-image-source"] = 42
	if policy.Invocation().SourceVersions["synthetic-image-source"] != 2 {
		t.Fatal("returned invocation exposed mutable source proof")
	}
	for _, action := range []string{"missing_proof", "foreign_account", "missing_source", "revoked"} {
		invocation := boundVisualInvocation()
		check := func(context.Context) error { return nil }
		switch action {
		case "missing_proof":
			invocation.Authorization = ""
		case "foreign_account":
			invocation.Account = "other"
		case "missing_source":
			invocation.SourceVersions = nil
		case "revoked":
			check = func(context.Context) error { return errors.New("revoked") }
		}
		if _, err := f.manager.newBoundMediaVisualObservationPolicy(t.Context(), incoming.Images(), invocation, 2, check, "bound source", 0); err == nil {
			t.Fatal("invalid visual origin accepted", action)
		}
	}
}

func TestBoundMediaVisualPromptUsesUnicodeContract(t *testing.T) {
	f, native, incoming, ctx := mediaPolicyFixture(t, false)
	for _, length := range []int{3000, 8192, 8193} {
		prompt := strings.Repeat("画", length)
		policy, err := f.manager.newBoundMediaVisualObservationPolicy(ctx, incoming.Images(), boundVisualInvocation(), 2, func(context.Context) error { return nil }, prompt, 0)
		if length <= 8192 && (err != nil || policy.Parameters().Prompt != prompt) || length > 8192 && err == nil {
			t.Fatal("bound visual prompt used byte count or truncated input", length, err)
		}
		policy, err = f.manager.newMediaPluginPolicy(ctx, native.msg, prompt, incoming.Images(), 0)
		if length <= 8192 && (err != nil || policy.Parameters().Prompt != prompt) || length > 8192 && err == nil {
			t.Fatal("incoming visual prompt differs from public Unicode contract", length, err)
		}
	}
	if _, err := f.manager.newBoundMediaVisualObservationPolicy(ctx, incoming.Images(), boundVisualInvocation(), 2, func(context.Context) error { return nil }, string([]byte{255}), 0); err == nil {
		t.Fatal("invalid UTF-8 visual prompt accepted")
	}
}

func TestMediaVisualIncomingInvocationRetainsDynamicSuffix(t *testing.T) {
	_, _, policy, ctx := mediaPolicyFixture(t, false)
	old, capability := mediaJob(policy)
	policy.receivedSharePolicy.requestSuffix += "-next-video-window"
	current, _ := mediaJob(policy)
	if old.Context.RequestKey == current.Context.RequestKey || policy.Check(ctx, old, capability) == nil {
		t.Fatal("incoming continuation reused frozen invocation")
	}
	if err := policy.Check(ctx, current, capability); err != nil {
		t.Fatal("new incoming continuation suffix rejected", err)
	}
}

func TestBoundMediaVisualMissingPluginNeverFallsBackToCurrentModel(t *testing.T) {
	f := setup(t)
	f.manager.config.APIKey = "synthetic-legacy-key"
	_, err := f.manager.newBoundMediaVisualObservationPolicy(t.Context(), nil, boundVisualInvocation(), 2, func(context.Context) error { return nil }, "bound source", 0)
	if err == nil || !strings.Contains(err.Error(), "media_plugin_migration_required") {
		t.Fatal("missing media plugin fell back to current model", err)
	}
}

// Opt-in local isolation contract only: the model transport is synthetic and
// the source is host-bound PNG bytes. No provider, device or WeChat send occurs.
func TestLiveBoundMediaVisualIsolationContract(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in isolated media visual contract")
	}
	f, policy, valid := boundVisualFixture(t)
	helper, err := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := pluginruntime.NewController(policy.store, pluginruntime.InstalledExecutor(policy.store, pluginruntime.Options{Python: python, Pythonw: filepath.Join(filepath.Dir(python), "pythonw.exe"), WindowsHelper: helper, RuntimeRoots: []string{filepath.Dir(python)}}))
	if err != nil {
		t.Fatal(err)
	}
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	installed, err := policy.store.Installed(t.Context(), "media-understanding")
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(t.Context(), installed.Package.ID, installed.Revision); err != nil {
		t.Fatal("isolated visual runtime initialization failed before provider callback", err)
	}
	calls := 0
	policy.post = func(context.Context, string, map[string]string, json.RawMessage, int64, time.Duration) (json.RawMessage, error) {
		calls++
		return json.RawMessage(mediaReply), nil
	}
	text, err := policy.Observe(t.Context())
	if err != nil || text != "Synthetic pixels" || calls != 1 {
		t.Fatal("actual isolated bound visual call failed", text, calls, err)
	}
	if err := policy.CheckCompleted(t.Context()); err != nil {
		t.Fatal(err)
	}
	jobs, err := policy.store.Jobs(t.Context(), "media-understanding", 0, 10)
	if err != nil || len(jobs.Items) != 1 || jobs.Items[0].State != "completed" || bytes.Contains(jobs.Items[0].Result, []byte("Synthetic pixels")) || jobs.Items[0].Context.Thread != "host:synthetic-visual-origin" {
		t.Fatal("isolated visual checkpoint lost host origin/privacy", jobs, err)
	}
	*valid = false
	if err := policy.CheckCompleted(t.Context()); err == nil {
		t.Fatal("actual completed observation ignored source revocation")
	}
}
