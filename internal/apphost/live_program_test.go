package apphost

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"wechat-bot/internal/pluginhost"
)

func TestLiveProgramDurableReferenceAndReplay(t *testing.T) {
	f := newLiveTestFixture(t)
	in := LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "program-replay", DurationSeconds: 60,
		ProgramArtifactID: "work-synthetic", ProgramSHA256: strings.Repeat("a", 64), ProgramLoop: true}
	first, err := f.control.Start(f.ctx, liveTestOwner, in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.control.Stop(f.ctx, liveTestOwner, first.ID)
	if first.ProgramArtifactID != in.ProgramArtifactID || first.ProgramSHA256 != in.ProgramSHA256 || !first.ProgramLoop {
		t.Fatalf("program not in view: %#v", first)
	}
	ref := currentLiveReference(t, f, first.ID)
	if ref.ProgramArtifactID != in.ProgramArtifactID || ref.ProgramSHA256 != in.ProgramSHA256 || !ref.ProgramLoop {
		t.Fatalf("program not durable: %#v", ref)
	}
	restarted := NewLiveController(f.control.registry, f.store, f.output)
	replay, err := restarted.Start(f.ctx, liveTestOwner, in)
	if err != nil || replay != first {
		t.Fatalf("replay changed program: %#v %v", replay, err)
	}
	for _, change := range []func(*LiveStart){
		func(s *LiveStart) { s.ProgramArtifactID = "work-other" },
		func(s *LiveStart) { s.ProgramSHA256 = strings.Repeat("b", 64) },
		func(s *LiveStart) { s.ProgramLoop = false },
	} {
		changed := in
		change(&changed)
		if _, err := restarted.Start(f.ctx, liveTestOwner, changed); !errors.Is(err, ErrLiveConflict) {
			t.Fatalf("changed program replay accepted: %v", err)
		}
	}
	if calls, _, _ := f.provider.calls(); calls != 1 {
		t.Fatalf("replay dispatched %d starts", calls)
	}
	state, err := readLive(f.store.LiveSession(f.ctx, first.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*LiveReference){
		func(r *LiveReference) { r.ProgramArtifactID = "work-other" },
		func(r *LiveReference) { r.ProgramSHA256 = strings.Repeat("b", 64) },
		func(r *LiveReference) { r.ProgramLoop = false },
	} {
		receipt := liveTestReceipt(state.Reference, "live")
		change(&receipt.Reference)
		if _, err := f.control.applyLiveReceipt(f.ctx, state, receipt); !errors.Is(err, ErrLiveConflict) {
			t.Fatalf("mismatched program receipt accepted: %v", err)
		}
	}
}

func TestLiveProgramValidationAndLegacyJSON(t *testing.T) {
	legacy := LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "legacy", DurationSeconds: 60}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"app_id":"synthetic-live","connection_id":"broadcast","request_key":"legacy","duration_seconds":60}`
	if string(raw) != want {
		t.Fatalf("legacy fingerprint input changed: %s", raw)
	}
	valid := legacy
	valid.ProgramArtifactID = "work-synthetic"
	valid.ProgramSHA256 = strings.Repeat("a", 64)
	for name, change := range map[string]func(*LiveStart){
		"missing hash":   func(s *LiveStart) { s.ProgramSHA256 = "" },
		"missing id":     func(s *LiveStart) { s.ProgramArtifactID = "" },
		"invalid id":     func(s *LiveStart) { s.ProgramArtifactID = "../file" },
		"uppercase hash": func(s *LiveStart) { s.ProgramSHA256 = strings.Repeat("A", 64) },
		"invalid hash":   func(s *LiveStart) { s.ProgramSHA256 = strings.Repeat("z", 64) },
		"short hash":     func(s *LiveStart) { s.ProgramSHA256 = strings.Repeat("a", 62) },
		"loop alone":     func(s *LiveStart) { s.ProgramArtifactID = ""; s.ProgramSHA256 = ""; s.ProgramLoop = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newLiveTestFixture(t)
			in := valid
			change(&in)
			if _, err := f.control.Start(f.ctx, liveTestOwner, in); !errors.Is(err, ErrLiveInput) {
				t.Fatalf("invalid program accepted: %v", err)
			}
			if calls, _, _ := f.provider.calls(); calls != 0 {
				t.Fatalf("invalid program dispatched %d starts", calls)
			}
			ref := LiveReference{ID: "synthetic", Target: ConnectionRef{AccountID: "account"}, Registration: "registered", GrantVersion: "grant", Deadline: time.Now().Add(time.Minute), ProgramArtifactID: in.ProgramArtifactID, ProgramSHA256: in.ProgramSHA256, ProgramLoop: in.ProgramLoop}
			state := liveState{Reference: ref, Owner: liveTestOwner, State: "starting"}
			payload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			record := pluginhost.LiveSessionRecord{ID: ref.ID, Active: true, Payload: payload}
			if _, err := decodeLive(record); !errors.Is(err, ErrLiveConflict) {
				t.Fatalf("invalid durable program accepted: %v", err)
			}
		})
	}
}
