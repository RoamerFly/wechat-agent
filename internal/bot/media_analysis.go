package bot

import (
	"context"
	"errors"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type messageAnalyzer interface {
	Analyze(context.Context, linuxnative.Message) (string, error)
}

type ScopedMediaAnalyzer interface {
	messageAnalyzer
	HandlesType(int) bool
	SupportsMedia(context.Context, linuxnative.Message) bool
}

func (w *Worker) hasScopedMedia(m linuxnative.Message) bool {
	return w.config.ScopedAnalyzer != nil && w.config.ScopedAnalyzer.HandlesType(m.Type)
}

func (w *Worker) mediaAnalyzer(ctx context.Context, m linuxnative.Message) messageAnalyzer {
	if w.hasScopedMedia(m) {
		if w.config.ScopedAnalyzer.SupportsMedia(ctx, m) {
			return w.config.ScopedAnalyzer
		}
		return nil
	}
	return w.config.Analyzer
}

// Persist the original media identity before invoking an external parser. The
// later observation updates this same source; it is never a new user statement.
// Without memory permission, preserve the existing ephemeral observation path.
func (w *Worker) analyzeIncomingMedia(ctx context.Context, message linuxnative.Message) (string, error) {
	text, _, err := w.analyzeIncomingMediaEvidence(ctx, message)
	return text, err
}

func (w *Worker) analyzeIncomingMediaEvidence(ctx context.Context, message linuxnative.Message) (string, MediaObservationEvidence, error) {
	// A batch may reuse the parent context; never inherit another item's proof.
	ctx = companion.WithMediaSource(ctx, companion.SourceRef{})
	if err := ctx.Err(); err != nil {
		return "", MediaObservationEvidence{}, err
	}
	analyzer := w.mediaAnalyzer(ctx, message)
	if analyzer == nil {
		return "", MediaObservationEvidence{}, errors.New("media_analyzer_unavailable")
	}
	e, ok := w.model.(*companion.Engine)
	if !ok || e.Store == nil || !e.Config.Permissions.Remember {
		return analyzeMediaWithEvidence(ctx, analyzer, message)
	}
	account := w.source.Account()
	if message.AccountID != "" && message.AccountID != account || message.ID == "" || message.ThreadID == "" {
		return "", MediaObservationEvidence{}, errors.New("media_source_scope_mismatch")
	}
	original := ""
	in := companion.Input{Channel: w.config.Channel, Account: account, Thread: message.ThreadID, Sender: message.SenderID, TraceID: message.ID, MessageTime: message.CreatedAt, Text: messageSummary(message), OriginalText: &original}
	bindInputSource(&in, message, e)
	source, found := e.Store.SourceForInput(in)
	if !found {
		if err := e.Store.Observe(in, e.Config, time.Now()); err != nil {
			return "", MediaObservationEvidence{}, err
		}
		source, found = e.Store.SourceForInput(in)
	}
	if !found {
		return "", MediaObservationEvidence{}, errors.New("source_identity_conflict_or_unavailable")
	}
	ctx = companion.WithMediaSource(ctx, *source)
	text, evidence, err := analyzeMediaWithEvidence(ctx, analyzer, message)
	if err != nil {
		return text, MediaObservationEvidence{}, err
	}
	if err = ctx.Err(); err != nil {
		return "", MediaObservationEvidence{}, err
	}
	in.Source = source
	if current, valid := e.Store.SourceForInput(in); !valid || current.EventID != source.EventID || current.Revision != source.Revision {
		return "", MediaObservationEvidence{}, errors.New("media_source_changed_during_analysis")
	}
	return text, evidence, nil
}

func analyzeMediaWithEvidence(ctx context.Context, analyzer messageAnalyzer, message linuxnative.Message) (string, MediaObservationEvidence, error) {
	if trusted, ok := analyzer.(interface {
		AnalyzeWithEvidence(context.Context, linuxnative.Message) (string, MediaObservationEvidence, error)
	}); ok {
		return trusted.AnalyzeWithEvidence(ctx, message)
	}
	text, err := analyzer.Analyze(ctx, message)
	return text, MediaObservationEvidence{}, err
}
