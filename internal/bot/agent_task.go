package bot

import (
	"context"
	"time"
	"wechat-bot/internal/companion"
)

func (w *Worker) taskActive(in companion.Input) bool {
	if w.config.DeliveryCancelled != nil {
		cancelled, err := w.config.DeliveryCancelled(in)
		if err != nil {
			w.persistErr = err
			return false
		}
		if cancelled {
			return false
		}
	}
	return in.TaskID == "" || w.config.AgentTaskActive == nil || w.config.AgentTaskActive(in)
}

func (w *Worker) tickAgentTask(ctx context.Context) bool {
	c, ok := w.model.(*companion.Engine)
	if !ok || w.runtime == nil || w.config.AgentTask == nil || w.draining() {
		return false
	}
	in, err := w.config.AgentTask(time.Now(), func(thread string) bool {
		return w.generation != nil && w.generation.jobs[thread] != nil || w.pendingConversationThread(thread)
	})
	if err != nil {
		w.config.Log("agent_task_schedule_failed")
		return false
	}
	if in == nil {
		return false
	}
	if !w.allowed[in.Thread] {
		if w.config.AuthorizeThread == nil {
			return false
		}
		allowed, e := w.config.AuthorizeThread(ctx, in.Thread)
		if e != nil || !allowed {
			return false
		}
		w.allowed[in.Thread] = true
	}
	in.PersonaOverride = w.config.GroupRules[in.Thread].Persona
	w.activity(Record{Event: "agent_task_started", Category: "decision", ThreadID: in.Thread, MessageID: in.TraceID, Status: "running", Reason: "persistent_task_continuation", Origin: "agent"})
	outcome := w.respondCharacter(ctx, c, *in)
	if w.generation == nil && outcome != "retrying_model" && w.config.AgentTaskDone != nil {
		if outcome == "" {
			outcome = "synchronous_round_finished"
		}
		w.config.AgentTaskDone(*in, outcome)
	}
	return true
}
