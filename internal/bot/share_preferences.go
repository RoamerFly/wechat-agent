package bot

import (
	"context"
	"errors"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// preferredGroupShare keeps the legacy preference-based behavior intact for
// older hosts. A configured station callback is authoritative: false means
// this exact card is not eligible for automatic handling, even if a stale
// free-form preference happens to mention sharing.
func (w *Worker) preferredGroupShare(m linuxnative.Message) bool {
	if w.config.PreferredGroupShare != nil {
		return w.config.ReadPreferredGroupShare != nil && w.config.PreferredGroupShare(m)
	}
	return w.considerPreferredShare(m)
}

// preparePreferredGroupShare is the narrow host-authorized automatic share
// path. Unlike the legacy Analyzer path it cannot run on a title/card alone:
// the source first becomes a durable, exact input, then the host reader must
// supply actual decoded evidence. Any failure stops the group turn before a
// model can invent a summary or emit a failure message into the group.
func (w *Worker) persistPreferredGroupShareSource(character *companion.Engine, in *companion.Input) error {
	if in == nil || in.Source == nil {
		return errors.New("share_group_source_unavailable")
	}
	if !in.HistoryObserved {
		if err := character.Store.Observe(*in, character.Config, time.Now()); err != nil {
			return err
		}
		in.HistoryObserved = true
	}
	resolved, ok := character.Store.SourceForInput(*in)
	if !ok {
		return errors.New("share_group_source_unavailable")
	}
	in.Source = resolved
	return nil
}

func (w *Worker) readPreparedPreferredGroupShare(ctx context.Context, character *companion.Engine, in *companion.Input, m linuxnative.Message) error {
	if w.config.ReadPreferredGroupShare == nil || in == nil || in.Source == nil {
		return errors.New("share_group_source_unavailable")
	}
	readCtx, cancel := context.WithTimeout(w.modelContext(ctx, m.ThreadID, m.ID), 3*time.Minute)
	summary, evidence, err := w.config.ReadPreferredGroupShare(readCtx, *in, m)
	cancel()
	if err != nil {
		return err
	}
	if strings.TrimSpace(summary) == "" || !companion.HasFirsthandObservation(evidence.Parts) {
		return errors.New("share_group_observation_not_firsthand")
	}
	if err := character.Store.CacheIncomingObservationWithCoverage(in.Channel, *in.Source, summary, evidence.ObservedRange, evidence.Excerpt, evidence.Parts...); err != nil {
		return err
	}
	for i := range in.IncomingMedia {
		in.IncomingMedia[i].Observation = summary
		in.IncomingMedia[i].ObservationState = "observed"
		in.IncomingMedia[i].ObservedRange = evidence.ObservedRange
		in.IncomingMedia[i].ObservationExcerpt = evidence.Excerpt
		in.IncomingMedia[i].ObservationParts = append([]companion.ObservationPart(nil), evidence.Parts...)
	}
	return nil
}

func (w *Worker) preparePreferredGroupShare(ctx context.Context, character *companion.Engine, in *companion.Input, m linuxnative.Message) error {
	if err := w.persistPreferredGroupShareSource(character, in); err != nil {
		return err
	}
	return w.readPreparedPreferredGroupShare(ctx, character, in, m)
}

// The model interprets administrator-authored free text in its existing turn;
// no keyword-to-permission conversion or extra classification model is used.
func (w *Worker) considerPreferredShare(m linuxnative.Message) bool {
	if m.Type != 49 || m.IsQuotedText() || m.FileAttachmentName() != "" || (m.Link == nil && (m.AppMessageType == nil || *m.AppMessageType != 51)) || m.Direction != "incoming" || !strings.HasSuffix(m.ThreadID, "@chatroom") || !w.allowed[m.ThreadID] || w.config.Analyzer == nil || w.config.Options == nil || !w.config.Options.ReadVideos {
		return false
	}
	e, ok := w.model.(*companion.Engine)
	if !ok || !e.Config.Enabled || !e.Config.Permissions.ObserveGroups || !e.Config.Permissions.AutonomousGroups || !e.Config.Permissions.ReplyText || e.Store == nil {
		return false
	}
	text, _ := e.Store.Preferences(companion.ScopeID(w.config.Channel, w.source.Account(), m.ThreadID))
	return strings.TrimSpace(text) != ""
}
