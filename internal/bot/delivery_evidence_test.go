package bot

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
)

func TestDeliveryEvidencePagesAreScopedStableAndExcludeContent(t *testing.T) {
	dir := t.TempDir()
	input := companion.Input{Account: "fixture", Thread: "room@chatroom", Text: "private request"}
	snapshot := runtimeState{Version: 1, Account: input.Account}
	for _, key := range []string{"third", "first", "second"} {
		snapshot.Deliveries = append(snapshot.Deliveries, &delivery{Key: key, State: "prepared", Thread: input.Thread,
			Input: input, SubmittedText: "private outgoing text"})
	}
	path := filepath.Join(dir, "messages-v2.json")
	if err := atomicState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	first, err := ReadDeliveryEvidencePage(dir, input.Account, "", 2)
	if err != nil || !reflect.DeepEqual(first.Keys, []string{"first", "second"}) || first.NextAfter != "second" || first.Snapshot == "" {
		t.Fatal(first, err)
	}
	last, err := ReadDeliveryEvidencePage(dir, input.Account, first.NextAfter, 2)
	if err != nil || !reflect.DeepEqual(last.Keys, []string{"third"}) || last.NextAfter != "" || last.Snapshot != first.Snapshot {
		t.Fatal(last, err)
	}
	for _, row := range first.Records {
		if row.Status.State != "unknown" {
			t.Fatal("possible submission was reported unsent", row)
		}
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "private") {
		t.Fatal("checkpoint text leaked", string(raw))
	}
	if _, err := ReadDeliveryEvidencePage(dir, "foreign-account", "", 2); err == nil {
		t.Fatal("foreign account checkpoint read")
	}
	if _, err := ReadDeliveryEvidencePage(dir, input.Account, "", 0); err == nil {
		t.Fatal("invalid limit accepted")
	}
	all, err := ReadDeliveryEvidencePage(dir, input.Account, "", 1000)
	if err != nil || len(all.Keys) != 3 || all.NextAfter != "" {
		t.Fatal(all, err)
	}
	snapshot.Deliveries[0].State = "stopped"
	if err := atomicState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	changed, err := ReadDeliveryEvidencePage(dir, input.Account, "", 2)
	if err != nil || changed.Snapshot == first.Snapshot {
		t.Fatal("snapshot change not identified", changed, err)
	}
	empty, err := ReadDeliveryEvidencePage(t.TempDir(), input.Account, "", 2)
	if err != nil || len(empty.Keys) != 0 {
		t.Fatal(empty, err)
	}
}

func TestDeliveryEvidenceNeverAssignsEarlierReceiptToRetryKey(t *testing.T) {
	dir := t.TempDir()
	in := companion.Input{Account: "fixture", Thread: "room@chatroom"}
	row := &delivery{Key: "retry-key", ID: "earlier-job", ServerID: "123", State: "accepted", Thread: in.Thread,
		Input: in, PreviousKeys: []string{"original-key"}}
	snapshot := runtimeState{Version: 1, Account: in.Account, Deliveries: []*delivery{row}}
	path := filepath.Join(dir, "messages-v2.json")
	if err := atomicState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	keys := []string{"original-key", "retry-key", "unrelated"}
	rows, err := ReadDeliveryEvidence(dir, in.Account, keys)
	if err != nil || len(rows) != 2 {
		t.Fatal("keys not preserved across retry", rows, err)
	}
	for _, key := range keys[:2] {
		if status := rows[key].Status; status.State != "unknown" || status.ID != "" || status.ServerID != "" || status.ThreadID != in.Thread {
			t.Fatal("ambiguous receipt assigned to an attempt", key, status)
		}
	}
	if _, err := ReadDeliveryEvidence(dir, "other-account", keys); err == nil {
		t.Fatal("foreign checkpoint was read")
	}
	snapshot.Deliveries = append(snapshot.Deliveries, row)
	if err := atomicState(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDeliveryEvidence(dir, in.Account, keys); err == nil {
		t.Fatal("duplicate attempt identity was resolved arbitrarily")
	}
}

func TestDeliveryEvidenceRetainsContinuationIdentity(t *testing.T) {
	dir := t.TempDir()
	in := companion.Input{Account: "fixture", Thread: "room"}
	snapshot := runtimeState{Version: 1, Account: in.Account, Media: []*mediaDelivery{{Key: "new-key", Trace: "new-request", Input: in, Kind: "voice", ResumedFrom: "old-key", ResumeOriginID: "old-job", State: "resume_prepared"}}}
	if err := atomicState(filepath.Join(dir, "messages-v2.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadDeliveryEvidence(dir, in.Account, []string{"new-key"})
	if err != nil || rows["new-key"].ResumedFrom != "old-key" || rows["new-key"].ResumeOriginID != "old-job" || rows["new-key"].Status.State != "prepared" {
		t.Fatal(rows, err)
	}
	snapshot.Media[0].State = "resume_submitting"
	if err := atomicState(filepath.Join(dir, "messages-v2.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	rows, err = ReadDeliveryEvidence(dir, in.Account, []string{"new-key"})
	if err != nil || rows["new-key"].Status.State != "unknown" {
		t.Fatal("possible continuation reported unsubmitted", rows, err)
	}
}
