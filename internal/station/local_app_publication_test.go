package station

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
)

// TestLocalPublicationNormalChatReplay exercises the public, normal local
// conversation path rather than constructing a Publisher directly.  It keeps
// the useful distinction between an editable project source and a deliverable
// artifact, but deliberately leaves the application target empty: a new fish
// must be able to prepare private work before any application is connected.
func TestLocalPublicationNormalChatReplay(t *testing.T) {
	f := localChatFixture(t)
	const sourceBody = "synthetic editable source: keep private"
	const artifactBody = "synthetic published artifact: keep private"
	sourceHash := fmt.Sprintf("%x", sha256.Sum256([]byte(sourceBody)))
	artifactHash := fmt.Sprintf("%x", sha256.Sum256([]byte(artifactBody)))

	var calls int
	var projectID, artifactRef, jobID string
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall("server_write", map[string]string{
					"path": "Project Tools/editable.txt", "content": sourceBody,
				}), nil
			case 2:
				return localToolCall(localArtifactPublishTool, map[string]string{
					"path": "Project Tools/editable.txt", "expected_sha256": sourceHash,
				}), nil
			case 3:
				return localToolCall("server_write", map[string]string{
					"path": "works/finished.txt", "content": artifactBody,
				}), nil
			case 4:
				return localToolCall(localArtifactPublishTool, map[string]string{
					"path": "works/finished.txt", "expected_sha256": artifactHash,
				}), nil
			case 5:
				ids := localPublicationWorkIDs(localModelHistory(turns))
				if len(ids) != 2 {
					return "", fmt.Errorf("expected two verified artifact IDs, got %v", ids)
				}
				projectID, artifactRef = ids[0], ids[1]
				return localToolCall(localPublicationToolName, map[string]any{
					"action": "draft", "title": "Synthetic draft", "body": "private body must not leak through list",
					"project_artifact_id": projectID, "artifact_ids": []string{artifactRef},
				}), nil
			default:
				history := localModelHistory(turns)
				for _, id := range localPublicationJobIDs(history) {
					jobID = id
				}
				if jobID == "" {
					t.Logf("draft tool feedback without job_id: %s", history)
					return "", fmt.Errorf("draft response did not expose job_id: %s", history)
				}
				return `{"action":"reply","messages":["作品草稿已保存。"]}`, nil
			}
		}), nil
	}

	if code, out := localChatSend(f, "publication-normal-chat", "做一份可编辑作品并保存草稿"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("draft chat: %d %#v", code, out)
	}
	if !artifactID(projectID, "work-") || !artifactID(artifactRef, "work-") || jobID == "" {
		t.Fatalf("normal chat did not return durable references: project=%q artifact=%q job=%q", projectID, artifactRef, jobID)
	}
	if calls != 6 {
		t.Fatalf("unexpected model/tool loop count: %d", calls)
	}

	// The same request id is an idempotent replay, not a second publication
	// draft (and must not invoke the model again).
	if code, out := localChatSend(f, "publication-normal-chat", "做一份可编辑作品并保存草稿"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("draft replay: %d %#v", code, out)
	}
	if calls != 6 {
		t.Fatalf("replay created another local draft, model calls=%d", calls)
	}

	code, listed := f.request("GET", "/api/local/publications?limit=1", nil, false)
	if code != http.StatusOK {
		t.Fatalf("list local drafts: %d %#v", code, listed)
	}
	assertLocalPublicationListing(t, listed, jobID, projectID, artifactRef)
	serialized := fmt.Sprintf("%v", listed)
	for _, forbidden := range []string{sourceBody, artifactBody, "Project Tools", "works/finished.txt", "origin"} {
		if strings.Contains(strings.ToLower(serialized), strings.ToLower(forbidden)) {
			t.Fatalf("publication list leaked private draft material %q: %s", forbidden, serialized)
		}
	}

	// No CSRF token means cancellation cannot be forged, even for a local-only
	// draft.
	if code, _ := f.request("POST", "/api/local/publications", map[string]string{"action": "cancel", "job_id": jobID}, false); code != http.StatusForbidden {
		t.Fatalf("publication cancel bypassed CSRF: %d", code)
	}
	reconcileInput := map[string]string{"action": "verify", "job_id": jobID, "bvid": "BV1aB2cD3e4F"}
	if code, _ := f.request("POST", "/api/local/publications/reconcile", reconcileInput, false); code != http.StatusForbidden {
		t.Fatalf("publication candidate read bypassed CSRF: %d", code)
	}
	if code, _ := f.request("POST", "/api/local/publications/reconcile", reconcileInput, true); code != http.StatusConflict {
		t.Fatalf("local draft permitted a platform candidate read: %d", code)
	}

	// A local-only draft has no provider to submit to.  It must fail without
	// changing into an accepted/remote task.
	if code, _ := f.request("POST", "/api/local/publications", map[string]string{"action": "submit", "job_id": jobID}, true); code != http.StatusForbidden {
		t.Fatalf("local draft unexpectedly submitted: %d", code)
	}

	// The durable task list is independent from the HTTP process lifetime.
	f = localRestartChat(t, f)
	code, listed = f.request("GET", "/api/local/publications?limit=1", nil, false)
	if code != http.StatusOK {
		t.Fatalf("list after restart: %d %#v", code, listed)
	}
	assertLocalPublicationListing(t, listed, jobID, projectID, artifactRef)

	// Forgetting a source immediately removes its artifact from new reads and
	// prevents a new source-backed continuation. Existing task status/cancel
	// remain available only to reconcile or stop that exact durable job.
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(companion.ScopeID("local", in.Account, in.Thread)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.home.artifacts.read(artifactRef); err == nil {
		t.Fatal("forgotten artifact remained readable")
	}
	if code, out := f.request("POST", "/api/local/publications", map[string]string{"action": "status", "job_id": jobID}, true); code != http.StatusOK || out["job_id"] != jobID {
		t.Fatalf("forgotten source blocked status reconciliation: %d %#v", code, out)
	}
	if code, _ := f.request("POST", "/api/local/publications", map[string]string{"action": "submit", "job_id": jobID}, true); code != http.StatusForbidden {
		t.Fatalf("forgotten local source permitted a new submit: %d", code)
	}
	if code, out := f.request("POST", "/api/local/publications", map[string]string{"action": "cancel", "job_id": jobID}, true); code != http.StatusOK || out["job_id"] != jobID {
		t.Fatalf("forgotten source blocked cancellation: %d %#v", code, out)
	}
}

func localPublicationWorkIDs(history string) []string {
	return localPublicationIDs(history, "work-")
}

func localPublicationJobIDs(history string) []string {
	seen := map[string]bool{}
	var ids []string
	for offset := 0; ; {
		start := strings.Index(history[offset:], "job_id")
		if start < 0 {
			break
		}
		start += offset + len("job_id")
		end := start
		for end+64 <= len(history) {
			candidate := history[end : end+64]
			if localPublicationHex(candidate) {
				if !seen[candidate] {
					seen[candidate] = true
					ids = append(ids, candidate)
				}
				break
			}
			end++
		}
		offset = start
	}
	return ids
}

func localPublicationHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func localPublicationIDs(history, prefix string) []string {
	seen := map[string]bool{}
	var ids []string
	for offset := 0; ; {
		start := strings.Index(history[offset:], prefix)
		if start < 0 {
			break
		}
		start += offset
		end := start + len(prefix)
		for end < len(history) {
			c := history[end]
			if !((c >= 'a' && c <= 'f') || (c >= '0' && c <= '9') || c == '-') {
				break
			}
			end++
		}
		id := history[start:end]
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
		offset = end
	}
	return ids
}

func assertLocalPublicationListing(t *testing.T, out map[string]any, jobID, projectID, artifactID string) {
	t.Helper()
	targets, ok := out["targets"].([]any)
	if !ok || len(targets) != 0 {
		t.Fatalf("unconnected home reported publication targets: %#v", out["targets"])
	}
	tasks, ok := out["tasks"].(map[string]any)
	if !ok {
		t.Fatalf("publication page missing: %#v", out)
	}
	items, ok := tasks["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("unexpected publication page: %#v", tasks)
	}
	item, ok := items[0].(map[string]any)
	if !ok || item["job_id"] != jobID || item["state"] != "local_draft" || item["project_id"] != projectID {
		t.Fatalf("wrong local draft view: %#v", item)
	}
	artifacts, ok := item["artifact_ids"].([]any)
	if !ok || len(artifacts) != 1 || artifacts[0] != artifactID {
		t.Fatalf("wrong safe artifact references: %#v", item)
	}
}
