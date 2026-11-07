package companion

import (
	"testing"
	"time"
)

func TestNativeRecallRespectsIdentityRevisionAndForgetting(t *testing.T) {
	for _, action := range []string{"unchanged", "sender", "time", "server", "text", "clear", "collision", "revision", "quarantine"} {
		t.Run(action, func(t *testing.T) {
			s, cfg, in := memoryFixture(t)
			now := time.Now().UTC()
			in.Text, in.MessageTime = "synthetic original", now.Format(time.RFC3339Nano)
			in.Source = &SourceRef{InstanceID: s.SourceInstanceID(), MessageID: in.TraceID, ServerID: "123", Revision: 1, OccurredAt: in.MessageTime}
			in.OriginalText = &in.Text
			if err := s.Observe(in, cfg, now); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "sender":
				in.Sender = "bob"
			case "time":
				in.MessageTime = now.Add(time.Second).Format(time.RFC3339Nano)
			case "server":
				in.Source.ServerID = "456"
			case "text":
				in.Text = "changed native body"
			case "clear":
				if err := s.Clear(ScopeID(in.Channel, in.Account, in.Thread)); err != nil {
					t.Fatal(err)
				}
			case "quarantine":
				var audit SourceAudit
				if err := s.db.QueryRow("SELECT id,scope,hash,revision FROM events").Scan(&audit.EventID, &audit.Scope, &audit.Hash, &audit.Revision); err != nil {
					t.Fatal(err)
				}
				if err := s.QuarantineSources(t.Context(), []SourceAudit{audit}); err != nil {
					t.Fatal(err)
				}
			case "collision", "revision":
				changed := in
				ref := *in.Source
				changed.Source = &ref
				changed.Text = "new body"
				changed.OriginalText = &changed.Text
				if action == "revision" {
					changed.Source.Revision = 2
				}
				if err := s.Observe(changed, cfg, now); err != nil {
					t.Fatal(err)
				}
			}
			allowed, err := s.NativeRecallAllowed(t.Context(), in)
			if err != nil || allowed != (action == "unchanged") {
				t.Fatal(action, allowed, err)
			}
		})
	}
}

func TestNativeRecallUnseenDoesNotImportOrChangeEpoch(t *testing.T) {
	s, _, in := memoryFixture(t)
	in.MessageTime = time.Now().UTC().Format(time.RFC3339Nano)
	before := recallEpochForTest(t, s)
	if allowed, err := s.NativeRecallAllowed(t.Context(), in); err != nil || !allowed {
		t.Fatal(allowed, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil || count != 0 || recallEpochForTest(t, s) != before {
		t.Fatal(count, err)
	}
}
