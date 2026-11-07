package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
	"wechat-bot/internal/agentrun"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

type backgroundCapabilityProbe struct {
	count     int
	reports   []companion.ContextReport
	followups []agentrun.Event
}

func (*backgroundCapabilityProbe) AutonomousActions() bool           { return true }
func (p *backgroundCapabilityProbe) ForkTools() companion.ToolAccess { return p }
func (p *backgroundCapabilityProbe) ContextPrepared(r companion.ContextReport) {
	p.reports = append(p.reports, r)
}
func (p *backgroundCapabilityProbe) ObserveFollowups(e []agentrun.Event) {
	p.followups = append(p.followups, e...)
}
func (*backgroundCapabilityProbe) List(context.Context) ([]mcptools.Tool, error) {
	return []mcptools.Tool{{Name: "server_read", InputSchema: json.RawMessage(`{"type":"object","properties":{"page":{"type":"integer"}},"required":["page"]}`)}}, nil
}
func (p *backgroundCapabilityProbe) Call(context.Context, string, json.RawMessage) (string, error) {
	p.count++
	return `{"state":"read"}`, nil
}

type backgroundCapabilityModel struct{ n int }

func (m *backgroundCapabilityModel) Chat(context.Context, string, []map[string]string) (string, error) {
	m.n++
	if m.n <= 35 {
		return fmt.Sprintf(`{"action":"tool","tool":"server_read","arguments":{"page":%d}}`, m.n), nil
	}
	return `{"action":"reply","messages":["模型完成了合成测试"]}`, nil
}

func TestBackgroundToolsPreserveAutonomyContextAndFollowups(t *testing.T) {
	probe := &backgroundCapabilityProbe{}
	w, c, n := backgroundWorker(t, &backgroundCapabilityModel{})
	c.Tools = probe
	c.MaxToolCalls = 1
	w.enableGenerations()
	defer w.closeGenerations()
	in := companion.Input{Channel: "wechat", Account: "test", Thread: "filehelper", Sender: "owner", TraceID: "capabilities", Mode: "direct", Text: "读取合成分页资料"}
	w.startSyntheticGeneration(t, context.Background(), c, in, nil)
	if len(w.generation.jobs) != 1 {
		t.Fatal("generation did not start")
	}
	pumpGeneration(t, w, func() bool { return len(w.generation.jobs) == 0 })
	readyPlans(w)
	w.reconcileUtterancePlans(context.Background())
	if probe.count != 35 || len(probe.reports) != 1 || !probe.reports[0].HasCurrent || len(n.submissions) != 1 {
		t.Fatal("background wrapper dropped a capability", probe.count, probe.reports, n.submissions)
	}
	wrapped := &generationTools{inner: probe}
	event := agentrun.Event{ID: "new", Text: "换成女声", Thread: in.Thread, Sender: in.Sender, Kind: 1}
	wrapped.ObserveFollowups([]agentrun.Event{event})
	if len(probe.followups) != 1 || probe.followups[0].ID != "new" {
		t.Fatal("followup observer was not forwarded")
	}
}

type isolatedReviewTools struct {
	generatedTools
	trace companion.Input
}

func (p *isolatedReviewTools) SetTrace(in companion.Input) { p.trace = in }
func (p *isolatedReviewTools) ForkTools() companion.ToolAccess {
	return &isolatedReviewTools{trace: p.trace}
}

func TestGenerationReviewForkPreservesCallbacksAndIsolatesTrace(t *testing.T) {
	base := &isolatedReviewTools{trace: companion.Input{Thread: "original-thread"}}
	scheduler, worker, job := &generationScheduler{}, &Worker{}, &generationJob{}
	wrapped := &generationTools{inner: base, scheduler: scheduler, worker: worker, job: job}
	fork, ok := wrapped.ForkTools().(*generationTools)
	if !ok || fork.inner == base || fork.scheduler != scheduler || fork.worker != worker || fork.job != job {
		t.Fatal("review fork lost isolation or actor callbacks")
	}
	fork.SetTrace(companion.Input{Thread: "review-thread"})
	if base.trace.Thread != "original-thread" || fork.inner.(*isolatedReviewTools).trace.Thread != "review-thread" {
		t.Fatal("review rewrote the running task's authorization identity")
	}
	if (&generationTools{inner: generatedTools{}}).ForkTools() != nil {
		t.Fatal("mutable wrapper manufactured a shared fallback fork")
	}
}

type ephemeralGenerationProbe struct{ backgroundCapabilityProbe }

func (*ephemeralGenerationProbe) EphemeralToolResult(name string) bool {
	return name == "fixture_authorized_history"
}

func TestGenerationEphemeralToolUsesLiveGateWithoutCheckpoint(t *testing.T) {
	w, _, _ := backgroundWorker(t, &backgroundCapabilityModel{})
	w.enableGenerations()
	defer w.closeGenerations()

	const name = "fixture_authorized_history"
	args := json.RawMessage(`{"page":1}`)
	in := companion.Input{Thread: "filehelper", TraceID: "ephemeral-history"}
	w.runtime.Inbox = []inboxItem{{Event: fixtureEvent("ephemeral-event", in.TraceID, in.Thread, "incoming", "remote")}}
	job := &generationJob{input: in}
	job.input.RecoveryToolResults = []companion.RecoveryToolResult{{Tool: name, CallKey: generationToolCallKey(name, args), Result: "stale-content"}}
	probe := &ephemeralGenerationProbe{}
	tools := &generationTools{inner: probe, worker: w, scheduler: w.generation, job: job}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type outcome struct {
		value string
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := tools.Call(ctx, name, args)
		done <- outcome{value, err}
	}()
	for {
		select {
		case request := <-w.generation.requests:
			w.runGenerationRequest(request)
		case result := <-done:
			if result.err != nil || result.value != `{"state":"read"}` || probe.count != 1 || len(w.runtime.Inbox[0].RecoveryTools) != 0 {
				t.Fatal("ephemeral result was replayed or persisted", result, probe.count)
			}
			return
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
