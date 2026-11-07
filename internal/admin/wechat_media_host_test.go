package admin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
)

func TestWechatMediaHostKeepsQueuePhasesAndOriginalIdentity(t *testing.T) {
	f, p, native := forwardFixture(t)
	defer p.hub.Close()
	raw := json.RawMessage(`{"source_thread_id":"source@chatroom","target_thread_id":"wxid_owner","message_id":"file-message","kind":"file","request_key":"original-batch-key"}`)
	h := &wechatMediaHost{p: p, raw: raw}
	in, err := wechatapp.ParseMediaRequest(raw, p.trace.Thread)
	if err != nil {
		t.Fatal(err)
	}
	changed := in
	changed.Target = "filehelper"
	if err := h.Authorize(t.Context(), changed); err == nil {
		t.Fatal("application changed original media destination")
	}
	if err := h.Authorize(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	var retained wechatapp.MediaSession
	result, err := h.ClaimMedia(t.Context(), in, func(s wechatapp.MediaSession) (any, error) {
		retained = s
		if _, err := s.Queue(t.Context()); err == nil {
			t.Fatal("queue before preparation")
		}
		if err := s.Prepare(t.Context()); err == nil {
			t.Fatal("prepare before source resolution")
		}
		if err := s.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := s.Prepare(t.Context()); err == nil {
			t.Fatal("application bypassed existing batch lookup")
		}
		if _, exists, err := s.Existing(t.Context()); err != nil || exists {
			t.Fatal(exists, err)
		}
		if err := s.Prepare(t.Context()); err != nil {
			t.Fatal(err)
		}
		out, err := s.Queue(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Queue(t.Context()); err == nil {
			t.Fatal("application queued one prepared batch twice")
		}
		return out, nil
	})
	if err != nil {
		t.Fatal(result, err)
	}
	if _, err := retained.Queue(t.Context()); err == nil {
		t.Fatal("application retained a closed queue session")
	}
	canonical, _ := json.Marshal(in)
	wantID := p.operationID("wechat_send_media", canonical)
	var out struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal([]byte(result), &out) != nil || out.ID != wantID {
		t.Fatal("canonical operation identity changed", result, wantID)
	}
	batch, err := f.manager.readCrossBatch(wantID)
	if err != nil || batch.Account != p.trace.Account || batch.Sender != p.trace.Sender || batch.Source != in.Source || batch.Target != in.Target || len(batch.Parts) != 1 {
		t.Fatal(batch, err)
	}
	if native.submits.Load() != 0 || native.downloads != 1 {
		t.Fatal("queueing sent early or fetched source twice", native.submits.Load(), native.downloads)
	}
}

func TestWechatMediaHostReusesAcceptedPersistentVoiceBatch(t *testing.T) {
	f, p, batch := crossRecoveryFixture(t, 3)
	defer p.hub.Close()
	for i, part := range batch.Parts {
		if err := f.manager.settleCrossMediaPart(batch.ID, part.Key, acceptedCrossPart(batch, i)); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(f.manager.dir, "cross-media", batch.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"asset_id": batch.Asset, "source_thread_id": batch.Source, "target_thread_id": batch.Target, "kind": "voice", "request_key": "fresh-key-same-accepted-batch"})
	result, err := p.Call(t.Context(), "wechat_send_media", raw)
	if err != nil || !strings.Contains(result, batch.ID) || !strings.Contains(result, `"state":"accepted"`) || !strings.Contains(result, `"reused_existing_operation":true`) {
		t.Fatal(result, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("accepted canonical journal or part keys changed", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || len(f.manager.takeMediaSeeds()) != 0 {
		t.Fatal("accepted parts were requeued", entries, err)
	}
}

func TestWechatMediaHostCancelledPreparationCannotPublishBatch(t *testing.T) {
	f, p, _ := forwardFixture(t)
	defer p.hub.Close()
	raw := json.RawMessage(`{"source_thread_id":"source@chatroom","target_thread_id":"wxid_owner","message_id":"file-message","kind":"file","request_key":"cancelled-batch-key"}`)
	h := &wechatMediaHost{p: p, raw: raw}
	in, err := wechatapp.ParseMediaRequest(raw, p.trace.Thread)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Authorize(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	_, err = h.ClaimMedia(t.Context(), in, func(s wechatapp.MediaSession) (any, error) {
		if err := s.Resolve(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := s.Existing(t.Context()); err != nil || exists {
			t.Fatal(exists, err)
		}
		if err := s.Prepare(t.Context()); err != nil {
			t.Fatal(err)
		}
		p.trace.TaskID = "no-longer-active-task"
		if _, err := s.Queue(t.Context()); err == nil {
			t.Fatal("cancelled media preparation was queued")
		}
		return map[string]any{"state": "cancelled"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(f.dir, "cross-media"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatal("cancelled preparation published a batch", files)
	}
}
