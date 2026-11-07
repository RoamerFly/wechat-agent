package station

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/comfyui"
	"wechat-bot/internal/companion"
)

type followupComfyFixture struct {
	syntheticLocalComfy
	statusUnavailable bool
}

func projectFollowupFixtureTools(t *testing.T, f *localHomeFixture, request string) *localToolSet {
	t.Helper()
	h := f.handler.chat
	store, in, err := h.conversation()
	if err != nil {
		t.Fatal(err)
	}
	in.TraceID = request
	original, err := store.LocalConversationRequest(t.Context(), in, request)
	if err != nil || original == nil {
		t.Fatal("original request unavailable", err)
	}
	in.Text, in.OriginalText, in.MessageTime = original.InputText, &original.InputText, original.OccurredAt
	var found bool
	in.Source, found = store.SourceForInput(in)
	if !found {
		t.Fatal("original request source missing")
	}
	tools := h.tools(store, in)
	tools.run, err = store.OpenLocalToolRun(t.Context(), in, false)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func (b *followupComfyFixture) Use(ctx context.Context, fish, binding string, start bool, fn func(localComfyClient) error) error {
	return b.syntheticLocalComfy.Use(ctx, fish, binding, start, func(localComfyClient) error { return fn(b) })
}

func (b *followupComfyFixture) Status(ctx context.Context, ref comfyui.JobRef) (comfyui.JobStatus, error) {
	if b.statusUnavailable {
		return comfyui.JobStatus{}, errors.New("synthetic temporary query failure")
	}
	return b.syntheticLocalComfy.Status(ctx, ref)
}

func TestLocalTaskFollowupCollectsProjectArtifactFromOriginalJob(t *testing.T) {
	f := localChatFixture(t)
	h := f.handler.chat
	store, in, err := h.conversation()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeLocalComfyChatWorkflow(t, root)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 48, 24))); err != nil {
		t.Fatal(err)
	}
	backend := &followupComfyFixture{syntheticLocalComfy: syntheticLocalComfy{root: root, fish: in.Account, lost: true,
		image: comfyui.Image{Data: data.Bytes(), SHA256: localComfyDigest(data.Bytes()), Width: 48, Height: 24}}}
	setUpLocalComfyChat(t, f, backend)
	const request = "compute-followup-original"
	jobPattern := regexp.MustCompile(`job_id[^a-f0-9]+([a-f0-9]{64})`)
	var modelRounds atomic.Int32
	h.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			history := localModelHistory(turns)
			switch modelRounds.Add(1) {
			case 1:
				return localToolCall(localComfyStartTool, map[string]any{"workflow_id": "smoke", "inputs": map[string]int{"width": 48}}), nil
			case 2:
				match := jobPattern.FindStringSubmatch(history)
				if len(match) != 2 {
					return "", errors.New("missing submitted job")
				}
				return localToolCall(localFollowupToolName, map[string]string{"action": "schedule", "result_tool": localComfyJobTool, "job_id": match[1]}), nil
			case 3:
				if !strings.Contains(history, "local-task-") {
					return "", errors.New("missing durable follow-up")
				}
				return `{"action":"reply","messages":["图像任务已登记。"]}`, nil
			case 4:
				if ids := projectArtifactIDs(turns); len(ids) != 2 || !strings.Contains(history, `"background_result":true`) {
					t.Errorf("project continuation did not receive verified artifacts: %v", ids)
					return "", errors.New("missing completion evidence")
				}
				return `{"action":"reply","messages":["两张图已生成。"]}`, nil
			default:
				return "", errors.New("unexpected model invocation")
			}
		}), nil
	}
	if code, out := localChatSend(f, request, "生成两张图，做好后告诉我"); code != 200 || out["status"] != "completed" {
		t.Fatal(code, out)
	}
	// Advance under the same turn lock as the worker, without relying on its
	// real timer. This also serializes mutations to the fixture provider.
	step := func(before func(), expectedState, expectedReason string) companion.LocalTaskFollowup {
		t.Helper()
		h.turn.Lock()
		defer func() {
			h.followups.mu.Lock()
			if h.followups.currentDone != nil {
				close(h.followups.currentDone)
				h.followups.currentDone = nil
			}
			h.followups.mu.Unlock()
			h.turn.Unlock()
		}()
		page, err := store.ListLocalTaskFollowups(t.Context(), in, "", 5)
		if err != nil || len(page.Items) != 1 {
			t.Fatal(page, err)
		}
		job, err := store.GetLocalTaskFollowup(t.Context(), in, page.Items[0].ID)
		if err != nil || job.JobID == "" || job.ResultTool != localComfyJobTool || job.RequestID != request {
			t.Fatal(job, err)
		}
		before()
		h.followups.advance(t.Context(), store, in, job)
		job, err = store.GetLocalTaskFollowup(t.Context(), in, job.ID)
		if err != nil || job.State != expectedState || job.Reason != expectedReason {
			t.Fatal(job, err)
		}
		if expectedState == "waiting" && !job.NextAt.After(time.Now()) {
			t.Fatal("waiting job has no future query time", job.NextAt)
		}
		return job
	}
	step(func() { backend.statusUnavailable = true }, "waiting", "project_status_retry")
	step(func() { backend.statusUnavailable = false }, "waiting", "waiting_for_project")
	if modelRounds.Load() != 3 {
		t.Fatal("polling a running project called the model", modelRounds.Load())
	}
	job := step(func() { backend.state = comfyui.Completed }, "completed", "result_reviewed")
	if modelRounds.Load() != 4 || backend.submits != 1 || backend.starts != 1 || backend.fetches != 2 {
		t.Fatal("unexpected compute replay or missing validated output", modelRounds.Load(), backend.submits, backend.starts, backend.fetches)
	}
	host, err := h.projectTools.forFish(in.Account)
	if err != nil {
		t.Fatal(err)
	}
	record, err := host.store.Job(t.Context(), job.JobID)
	if err != nil || record.State != "completed" || len(record.Artifacts) != 2 {
		t.Fatal(record.State, record.Artifacts, err)
	}
	for _, id := range record.Artifacts {
		if got := localArtifactDownload(t, f, id); !bytes.Equal(got, data.Bytes()) {
			t.Fatal("download differs from verified output", id)
		}
	}
	page, err := store.LocalConversationPage(t.Context(), in, "", 20)
	if err != nil || len(page.Items) != 3 || page.Items[0].Role != "user" || page.Items[2].Role != "assistant" {
		t.Fatal(page, err)
	}
	in.TraceID, in.Text, in.MessageTime = "unrelated-compute-request", "核对另一条任务", time.Now().UTC().Format(time.RFC3339Nano)
	in.OriginalText = &in.Text
	in.Source = &companion.SourceRef{MessageID: in.TraceID}
	if err := store.Observe(in, localConversationConfig(""), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tools := h.tools(store, in)
	tools.run, err = store.OpenLocalToolRun(t.Context(), in, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.projectFollowupJob(t.Context(), localComfyJobTool, job.JobID); !errors.Is(err, errProjectFollowupAuthority) {
		t.Fatal("another request could attach the original job", err)
	}
}
