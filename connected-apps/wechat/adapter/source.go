package adapter

import (
	"context"
	"errors"

	"wechat-bot/internal/apphost"
)

// SourceMetadata is the application-facing source contract. Host-only hashes,
// import/revision bookkeeping and raw memory models are deliberately absent.
type SourceMetadata struct {
	EventID, InstanceID, Account, Thread, MessageID, ServerID, Sender string
	OccurredAt, ReceivedAt                                            string
	Revision, MessageType                                             int
	Sequence                                                          int64
}

// SourceRecord contains metadata only. The host reads the authoritative store,
// applies forgetting/version barriers and binds this reader to one account.
// Neither conversation bodies nor private storage handles cross this boundary.
type SourceRecord struct {
	Source         SourceMetadata
	StoredSequence int64
	AddresseeIDs   []string
	QuotedSource   *SourceMetadata
	Media          []SourceMedia
}

type SourceMedia struct {
	EventID  string
	Revision int
	AssetID  string
	Kind     string
}

type SourceReader func(context.Context, string, string) (SourceRecord, error)

func sourceRef(record SourceRecord) apphost.EventRef {
	src := record.Source
	return apphost.EventRef{EventID: src.EventID, MessageID: src.MessageID, ServerID: src.ServerID,
		SenderID: src.Sender, Revision: src.Revision}
}

func readSource(ctx context.Context, read SourceReader, account, thread, message string) (SourceRecord, error) {
	if read == nil {
		return SourceRecord{}, apphost.ErrUnavailable
	}
	record, err := read(ctx, thread, message)
	if err != nil {
		return SourceRecord{}, err
	}
	src := record.Source
	if src.Account != account || src.Thread != thread || src.MessageID != message || src.EventID == "" || src.InstanceID == "" || src.Revision <= 0 {
		return SourceRecord{}, apphost.ErrNotFound
	}
	return record, nil
}

// ReadEvent projects the existing WeChat source and exact quote/media ancestry.
// Authorization and source lookup always come from the host; the application
// cannot select a different account or a raw memory database.
func ReadEvent(ctx context.Context, account, thread, message string, authorize func(context.Context, string) (bool, error), read SourceReader) (apphost.Event, error) {
	if account == "" || thread == "" || message == "" || len(message) > 8192 {
		return apphost.Event{}, apphost.ErrNotFound
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if authorize == nil {
			return apphost.ErrNotFound
		}
		allowed, err := authorize(ctx, thread)
		if err != nil {
			return err
		}
		if !allowed {
			return apphost.ErrNotFound
		}
		return nil
	}
	if err := check(); err != nil {
		return apphost.Event{}, err
	}
	record, err := readSource(ctx, read, account, thread, message)
	if err != nil {
		return apphost.Event{}, err
	}
	src := record.Source
	out := apphost.Event{
		ConnectionRef: ConnectionRef(account), EventRef: sourceRef(record),
		ConversationID: thread, DatasetID: src.InstanceID, MessageType: src.MessageType,
		OccurredAt: src.OccurredAt, ReceivedAt: src.ReceivedAt,
		StoredSequence: record.StoredSequence, SourceSequence: src.Sequence,
		AddresseeIDs: append([]string(nil), record.AddresseeIDs...), ReadScope: "source_metadata",
	}
	if q := record.QuotedSource; q != nil && q.Account == account && q.Thread == thread && q.InstanceID == src.InstanceID {
		if err := check(); err != nil {
			return apphost.Event{}, err
		}
		quoted, err := readSource(ctx, read, account, thread, q.MessageID)
		if err != nil && !errors.Is(err, apphost.ErrNotFound) {
			return apphost.Event{}, err
		}
		if err == nil && quoted.Source.InstanceID == src.InstanceID && q.ServerID != "" && q.ServerID == quoted.Source.ServerID && q.Sender == quoted.Source.Sender &&
			q.Revision == quoted.Source.Revision && (q.EventID == "" || q.EventID == quoted.Source.EventID) {
			ref := sourceRef(quoted)
			out.Quote = &ref
		}
	}
	for _, media := range record.Media {
		if media.EventID == src.EventID && media.Revision == src.Revision {
			out.Media = append(out.Media, apphost.MediaRef{AssetID: media.AssetID, Kind: media.Kind})
		}
	}
	if err := check(); err != nil {
		return apphost.Event{}, err
	}
	return out, nil
}
