package station

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
)

func TestLocalArtifactPublishComputesHashAndRecoversWithoutExecutor(t *testing.T) {
	f := localChatFixture(t)
	_, home := f.request("GET", "/api/local/home", nil, false)
	path := filepath.Join(home["directory"].(string), "works", "auto.txt")
	const body = "synthetic output without a separate hashing process"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	publish := localToolCall(localArtifactPublishTool, map[string]string{"path": "works/auto.txt"})
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(context.Context, string, []map[string]string) (string, error) {
			calls++
			if calls == 1 {
				return publish, nil
			}
			return "", fmt.Errorf("synthetic model interruption after verified snapshot")
		}), nil
	}
	if code, _ := localChatSend(f, "auto-hash-work", "登记现有成品"); code != 502 {
		t.Fatal(code)
	}
	if err := os.WriteFile(path, []byte("changed mutable file"), 0600); err != nil {
		t.Fatal(err)
	}
	next := localRestartChat(t, f)
	next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			calls++
			if calls == 1 {
				return publish, nil
			}
			history := localModelHistory(turns)
			if !strings.Contains(history, sha256Hex([]byte(body))) || !strings.Contains(history, "work-") {
				t.Error("verified original snapshot and actual hash missing from recovery")
			}
			return `{"action":"reply","messages":["已登记原成品。"]}`, nil
		}), nil
	}
	if code, out := localChatSend(next, "auto-hash-work", "登记现有成品"); code != 200 {
		t.Fatal(code, out)
	}
}

func TestLocalArtifactsPublishThroughChatAndRetainVerifiedSnapshot(t *testing.T) {
	f := localChatFixture(t)
	const body = "a synthetic finished document"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	calls := 0
	var id string
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, _ string, history []map[string]string) (string, error) {
			calls++
			switch calls {
			case 1:
				return fmt.Sprintf(`{"action":"tool","tool":"server_write","arguments":{"path":"Project Tools/finished.txt","content":%q}}`, body), nil
			case 2:
				return fmt.Sprintf(`{"action":"tool","tool":"artifact_publish","arguments":{"path":"Project Tools/finished.txt","expected_sha256":%q}}`, hash), nil
			default:
				for _, turn := range history {
					start := strings.Index(turn["content"], "work-")
					if start >= 0 && len(turn["content"]) >= start+69 {
						id = turn["content"][start : start+69]
					}
				}
				return `{"action":"reply","messages":["作品已登记。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "publish-work", "写一份文档并登记成品"); code != 200 {
		t.Fatal(code, out)
	}
	if !artifactID(id, "work-") {
		t.Fatal("chat did not receive verified artifact ID", id)
	}
	artifact, err := f.handler.home.artifacts.read(id)
	if err != nil || artifact.Source == nil || artifact.Request != "publish-work" {
		t.Fatal("artifact lost source", artifact, err)
	}
	_, home := f.request("GET", "/api/local/home", nil, false)
	path := filepath.Join(home["directory"].(string), "Project Tools", "finished.txt")
	if err := os.WriteFile(path, []byte("changed later"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := f.handler.home.artifacts.ReadArtifact(id)
	if err != nil || ref.ID != id || ref.Name != artifact.Name || ref.SHA256 != hash {
		t.Fatalf("unexpected application artifact: %+v %v", ref.View(), err)
	}
	if file, err := ref.OpenVerified(t.Context(), func() bool { return false }); err == nil {
		file.Close()
		t.Fatal("application received the bytes after its delivery fence closed")
	}
	original, err := ref.OpenVerified(t.Context(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, exposesPath := original.(interface{ Name() string }); exposesPath {
		original.Close()
		t.Fatal("application received the private file path")
	}
	got, err := io.ReadAll(original)
	original.Close()
	if err != nil || string(got) != body {
		t.Fatal("snapshot followed mutable source", string(got), err)
	}
	reused, err := f.handler.home.artifacts.publish(t.Context(), localArtifactInput{Path: "Project Tools/finished.txt", Expected: hash, Key: "publish-work"}, artifact.Source)
	if err != nil || reused.ID != id {
		t.Fatal("retry lost immutable artifact", reused, err)
	}
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	f = newLocalHomeFixture(t, f.dir, f.root)
	f.login()
	if _, err := f.handler.home.artifacts.read(id); err != nil {
		t.Fatal("restart lost source or artifact", err)
	}
	ref, err = f.handler.home.artifacts.ReadArtifact(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"../outside.txt", filepath.Join(f.dir, "fish-identity.json")} {
		code, _ := f.request("POST", "/api/local/artifacts", map[string]string{"path": invalid, "request_key": "invalid-path"}, true)
		if code != 400 {
			t.Fatal("private path accepted", code)
		}
	}
	if code, _ := f.request("POST", "/api/local/artifacts", map[string]string{"path": "Project Tools/finished.txt", "expected_sha256": hash, "request_key": "mismatch"}, true); code != 400 {
		t.Fatal("changed hash accepted", code)
	}
	_, current := f.request("GET", "/api/local/home", nil, false)
	if code, out := f.request("POST", "/api/local/home", map[string]any{"action": "pause", "revision": current["revision"]}, true); code != 200 {
		t.Fatal(code, out)
	}
	if _, err := f.handler.home.artifacts.read(id); err == nil {
		t.Fatal("home pause did not stop access")
	}
	if file, err := ref.OpenVerified(t.Context(), func() bool { return true }); err == nil {
		file.Close()
		t.Fatal("cached application artifact bypassed home pause")
	}
	_, paused := f.request("GET", "/api/local/home", nil, false)
	if code, out := f.request("POST", "/api/local/home", map[string]any{"action": "authorize", "revision": paused["revision"]}, true); code != 200 {
		t.Fatal(code, out)
	}
	private := filepath.Join(f.dir, "artifacts", id, "original")
	if err := os.WriteFile(private, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if file, err := ref.OpenVerified(t.Context(), func() bool { return true }); err == nil {
		file.Close()
		t.Fatal("corrupt snapshot accepted")
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(companion.ScopeID("local", in.Account, in.Thread)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.home.artifacts.read(id); err == nil {
		t.Fatal("forgotten source remained publishable")
	}
}

func TestLocalArtifactPublishRecoveryReadsOriginalAndRejectsNewSnapshot(t *testing.T) {
	f := localChatFixture(t)
	_, home := f.request("GET", "/api/local/home", nil, false)
	works := filepath.Join(home["directory"].(string), "works")
	for _, name := range []string{"first.txt", "extra.txt"} {
		if err := os.WriteFile(filepath.Join(works, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash := sha256Hex([]byte("first.txt"))
	publish := localToolCall(localArtifactPublishTool, map[string]string{"path": "works/first.txt", "expected_sha256": hash})
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, messages []map[string]string) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall(localArtifactPublishTool, map[string]string{"path": "works/first.txt", "expected_sha256": strings.Repeat("0", 64)}), nil
			case 2:
				if !strings.Contains(localModelHistory(messages), "not_published") {
					t.Error("wrong digest should be a known rejection")
				}
				return publish, nil
			default:
				return "", fmt.Errorf("synthetic interruption after artifact publication")
			}
		}), nil
	}
	if code, _ := localChatSend(f, "resume-publish", "登记首份成品"); code != 502 {
		t.Fatalf("expected a failed model response after tool execution: %d", code)
	}
	artifacts := filepath.Join(f.dir, "artifacts")
	before, err := os.ReadDir(artifacts)
	if err != nil || len(before) != 1 || !artifactID(before[0].Name(), "work-") {
		t.Fatalf("initial artifact missing: %v %v", before, err)
	}
	if err := os.Remove(filepath.Join(works, "first.txt")); err != nil {
		t.Fatal(err)
	}
	artifact := before[0].Name()
	f = localRestartChat(t, f)
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		calls := 0
		return localModelFunc(func(_ context.Context, _ string, messages []map[string]string) (string, error) {
			calls++
			history := localModelHistory(messages)
			switch calls {
			case 1:
				if !strings.Contains(history, "tool_recovery") || !strings.Contains(history, artifact) {
					t.Error("restart did not supply the original artifact receipt")
				}
				return publish, nil
			case 2:
				if !strings.Contains(history, `"recovered":true`) || !strings.Contains(history, hash) {
					t.Error("repeating the same operation did not recover the original artifact")
				}
				return localToolCall(localArtifactPublishTool, map[string]string{"path": "works/extra.txt", "expected_sha256": sha256Hex([]byte("extra.txt"))}), nil
			default:
				if !strings.Contains(history, "新增写入和启动需要主人发起新请求") {
					t.Error("resume attempted an unrelated artifact publication")
				}
				return `{"action":"reply","messages":["已找到原成品。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "resume-publish", "登记首份成品"); code != 200 {
		t.Fatalf("resume: %d %#v", code, out)
	}
	after, err := os.ReadDir(artifacts)
	if err != nil || len(after) != 1 || after[0].Name() != artifact || string(localArtifactDownload(t, f, artifact)) != "first.txt" {
		t.Fatalf("resume changed the immutable snapshot: %v %v", after, err)
	}
}
