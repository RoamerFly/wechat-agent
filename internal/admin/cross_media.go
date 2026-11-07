package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

type crossMediaInput struct {
	Conversion *voiceConversion `json:"conversion,omitempty"`
	Source     string           `json:"source_thread_id"`
	Target     string           `json:"target_thread_id"`
	Asset      string           `json:"asset_id"`
	Message    string           `json:"message_id"`
	Kind       string           `json:"kind"`
	Key        string           `json:"request_key"`
}
type crossMediaBatch struct {
	OriginInput     *companion.Input                  `json:"origin_input,omitempty"`
	Conversion      *voiceConversion                  `json:"conversion,omitempty"`
	RequestIdentity string                            `json:"request_identity,omitempty"`
	SupersededBy    string                            `json:"superseded_by,omitempty"`
	CancelledAt     time.Time                         `json:"cancelled_at,omitempty"`
	TaskID          string                            `json:"task_id,omitempty"`
	Receipts        map[string]linuxnative.ToolStatus `json:"receipts,omitempty"`
	ID              string                            `json:"operation_id"`
	Account         string                            `json:"account"`
	Origin          string                            `json:"origin_thread_id"`
	Source          string                            `json:"source_thread_id"`
	Target          string                            `json:"target_thread_id"`
	Sender          string                            `json:"requester_id"`
	Asset           string                            `json:"asset_id"`
	Created         time.Time                         `json:"created"`
	Parts           []bot.MediaSeed                   `json:"parts"`
}

func validCrossReceipt(batch crossMediaBatch, part bot.MediaSeed, status linuxnative.ToolStatus) bool {
	return status.State == "accepted" && status.ServerID != "" && status.ThreadID == batch.Target && (status.SourceID == "" || status.SourceID == part.SourceID)
}

// The durable batch is the canonical route. A separately checkpointed worker
// queue must never override account, origin, target, or part identity.
func validateCrossBatch(batch crossMediaBatch, account string) error {
	if !validID(batch.ID) || batch.Account != account || batch.Origin == "" || batch.Source == "" || batch.Target == "" || batch.Sender == "" || batch.Asset == "" || len(batch.Parts) == 0 {
		return errors.New("invalid_cross_media_journal")
	}
	if err := batch.Conversion.validate(batch.Parts[0].Media.Kind); err != nil {
		return err
	}
	if batch.OriginInput != nil && (batch.OriginInput.Account != batch.Account || batch.OriginInput.Thread != batch.Origin || batch.OriginInput.Sender != batch.Sender || batch.OriginInput.TaskID != batch.TaskID) {
		return errors.New("cross_media_origin_identity_mismatch")
	}
	keys := map[string]bool{}
	for _, p := range batch.Parts {
		source := p.SourceThread
		if source == "" {
			source = p.OriginThread
		}
		if !validID(p.Key) || keys[p.Key] || p.SourceID == "" || p.Trace != batch.ID || p.Input.TraceID != "" && p.Input.TraceID != batch.ID || p.Input.Account != batch.Account || p.Input.Thread != batch.Target || p.OriginThread != batch.Origin || source != batch.Source || p.Requester != batch.Sender || p.OriginTask != batch.TaskID || !contains([]string{"voice", "file", "image", "video"}, p.Media.Kind) {
			return errors.New("cross_media_part_identity_mismatch")
		}
		if !sameCrossOrigin(p.OriginInput, batch.OriginInput) {
			return errors.New("cross_media_origin_proof_mismatch")
		}
		keys[p.Key] = true
		if status, ok := batch.Receipts[p.Key]; ok && !validCrossReceipt(batch, p, status) {
			return errors.New("cross_media_receipt_identity_mismatch")
		}
	}
	for key := range batch.Receipts {
		if !keys[key] {
			return errors.New("cross_media_receipt_unknown_part")
		}
	}
	return nil
}

func crossBatchAccepted(batch crossMediaBatch) bool {
	if len(batch.Parts) == 0 {
		return false
	}
	for _, part := range batch.Parts {
		status, ok := batch.Receipts[part.Key]
		if !ok || !validCrossReceipt(batch, part, status) {
			return false
		}
	}
	return true
}

func (p *permittedTools) crossAllowed(source, target string) bool {
	return p.privileged() && p.manager.agent.config().CrossConversation && p.manager.accountThreadKnown(source) && p.manager.accountThreadKnown(target)
}
func (p *permittedTools) sendMedia(ctx context.Context, args json.RawMessage) (string, error) {
	return wechatapp.SendMedia(ctx, args, &wechatMediaHost{p: p, raw: slices.Clone(args)})
}

// The destination input is only a route. Preserve evidence in the originating
// request's actual scope, before conversion/upload, and never re-stamp on resume.
func (p *permittedTools) prepareCrossMediaOrigin(ctx context.Context) (*companion.Input, error) {
	origin := p.outboundInput(p.trace.Thread)
	if origin.OriginTraceID == "" {
		origin.OriginTraceID = outboundAnchor(origin)
	}
	r := companion.ReplyContext{TargetIDs: []string{origin.Sender}, AnchorMessageID: outboundAnchor(origin), SourceVersions: map[string]int{}}
	if origin.Source != nil && origin.Source.EventID != "" {
		r.SourceEventIDs = []string{origin.Source.EventID}
		r.SourceVersions[origin.Source.EventID] = origin.Source.Revision
	}
	r, err := p.stampOutboundBinding(ctx, origin, r)
	if err != nil {
		return nil, rejectOutboundEvidence(err)
	}
	origin.ReplyContext = &r
	if err := p.validateCrossMediaOrigin(ctx, &origin); err != nil {
		return nil, err
	}
	// Detach the durable snapshot from later SetTrace/media observation updates.
	raw, err := json.Marshal(origin)
	if err != nil {
		return nil, err
	}
	var snapshot companion.Input
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (p *permittedTools) validateCrossMediaOrigin(ctx context.Context, origin *companion.Input) error {
	if origin == nil || origin.ReplyContext == nil || origin.Account != p.trace.Account || origin.Thread != p.trace.Thread || origin.Sender != p.trace.Sender {
		return rejectOutboundEvidence(errors.New("cross_media_origin_evidence_requires_review"))
	}
	input := *origin
	if input.TraceID == "" {
		input.TraceID = outboundAnchor(input)
		if input.TraceID == "" {
			input.TraceID = input.OriginTraceID
		}
	}
	return rejectOutboundEvidence(validateOutboundSource(ctx, p.manager, input, *input.ReplyContext, p))
}

func sameCrossOrigin(a, b *companion.Input) bool {
	// TraceID and other process-only fields are deliberately not persisted by
	// companion.Input. Canonical identity compares the complete durable shape.
	left, e1 := json.Marshal(a)
	right, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(left) == string(right)
}

func (p *permittedTools) validateSelectedMediaVoice(media companion.GeneratedMedia) error {
	voice, profile := p.SelectedVoiceIdentity()
	if voice == "" && profile == "" {
		return nil
	}
	if media.Kind != "voice" && !strings.HasPrefix(media.CachedMIME, "audio/") {
		return nil
	}
	if voice != "" && media.VoiceID != voice || profile != "" && media.VoiceProfile != profile {
		return toolInputError("named_voice_result_identity_mismatch: 待发送音频未匹配本任务声线，未入队；请读取原任务并使用指定voice_id/profile继续")
	}
	return nil
}
func (m *Manager) queueCrossBatch(batch crossMediaBatch) error {
	if e := validateCrossBatch(batch, m.native.Account()); e != nil {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.mediaSeeds) == 0 {
		if b, e := os.ReadFile(filepath.Join(m.dir, "media-recovery.json")); e == nil {
			if e = json.Unmarshal(b, &m.mediaSeeds); e != nil {
				return e
			}
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	keys := map[string]bool{}
	for _, seed := range batch.Parts {
		keys[seed.Key] = true
	}
	next := []bot.MediaSeed{}
	for _, old := range m.mediaSeeds {
		if old.Trace != batch.ID && !keys[old.Key] {
			next = append(next, old)
		}
	}
	for _, seed := range batch.Parts {
		if batch.SupersededBy != "" || !batch.CancelledAt.IsZero() {
			continue
		}
		if status, ok := batch.Receipts[seed.Key]; ok && validCrossReceipt(batch, seed, status) {
			continue
		}
		seed.Input.TraceID = seed.Trace
		seed.Media.SourceID = seed.SourceID
		next = append(next, seed)
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if e = writePrivate(filepath.Join(m.dir, "media-recovery.json"), b); e != nil {
		return e
	}
	m.mediaSeeds = next
	return nil
}

// Repair a crash between the durable batch journal, parent-task binding and
// queue handoff. Only still-authorized original operations are adopted; no
// source conversion, new upload or new send identity is created here.
func (m *Manager) restoreCrossMedia() error {
	entries, err := os.ReadDir(filepath.Join(m.dir, "cross-media"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	batches := []crossMediaBatch{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			continue
		}
		data, e := os.ReadFile(filepath.Join(m.dir, "cross-media", entry.Name()))
		if e != nil {
			return e
		}
		var batch crossMediaBatch
		if json.Unmarshal(data, &batch) != nil || batch.ID != id {
			if e = m.blockCrossRecovery(id, "invalid_cross_media_journal"); e != nil {
				return e
			}
			continue
		}
		if e = validateCrossBatch(batch, m.native.Account()); e != nil {
			if e = m.blockCrossRecovery(id, e.Error()); e != nil {
				return e
			}
			continue
		}
		batches = append(batches, batch)
	}
	sort.SliceStable(batches, func(i, j int) bool { return batches[i].Created.Before(batches[j].Created) })
	for _, batch := range batches {
		if batch.SupersededBy != "" || !batch.CancelledAt.IsZero() {
			if err = m.queueCrossBatch(batch); err != nil {
				return err
			}
			if err = m.settleCrossTask(batch); err != nil {
				return err
			}
			continue
		}
		if crossBatchAccepted(batch) {
			// Receipt commit can precede queue and parent-task checkpoints.
			// Complete that handoff without submitting any part again, even if
			// sending permission was subsequently revoked.
			if err = m.queueCrossBatch(batch); err != nil {
				return err
			}
			if err = m.settleCrossTask(batch); err != nil {
				return err
			}
			continue
		}
		if !m.crossSeedAllowed(batch.Parts[0]) {
			// Preserve canonical keys for read-only native receipt recovery.
			// MediaSeedKnown permits adoption, while MediaSeedAllowed still
			// prevents Submit/Retry for missing, revoked or stale source proof.
			if err = m.queueCrossBatch(batch); err != nil {
				return err
			}
			continue
		}
		if batch.TaskID != "" {
			if err = m.bindCrossTask(batch); err != nil {
				if err.Error() == "cross_task_scope_mismatch" {
					if err = m.blockCrossRecovery(batch.ID, "cross_task_scope_mismatch"); err != nil {
						return err
					}
					continue
				}
				return err
			}
		}
		if err = m.queueCrossBatch(batch); err != nil {
			return err
		}
	}
	return nil
}
func (p *permittedTools) crossMediaStatus(ctx context.Context, id string) (string, error) {
	if !validID(id) {
		return "", toolInputError("invalid_cross_media_operation_id")
	}
	b, e := os.ReadFile(filepath.Join(p.manager.dir, "cross-media", id+".json"))
	if e != nil {
		return "", e
	}
	var batch crossMediaBatch
	if json.Unmarshal(b, &batch) != nil || batch.Account != p.trace.Account || batch.Origin != p.trace.Thread || batch.Sender != p.trace.Sender || !p.crossAllowed(batch.Source, batch.Target) {
		return "", toolInputError("cross_media_scope_mismatch")
	}
	if e = validateCrossBatch(batch, p.manager.native.Account()); e != nil {
		return "", e
	}
	if !batch.CancelledAt.IsZero() {
		out, _ := json.Marshal(map[string]any{"operation_id": batch.ID, "state": "cancelled", "target_thread_id": batch.Target})
		return string(out), nil
	}
	if batch.SupersededBy != "" {
		out, _ := json.Marshal(map[string]any{"operation_id": batch.ID, "state": "superseded", "superseded_by": batch.SupersededBy, "target_thread_id": batch.Target, "instruction": "已合并到另一个投递批次，检查superseded_by；不要恢复重复发送。"})
		return string(out), nil
	}
	native, ok := p.manager.native.(nativeTools)
	if !ok {
		return "", errors.New("native_status_unavailable")
	}
	states := []linuxnative.ToolStatus{}
	all := true
	for _, part := range batch.Parts {
		status, known := batch.Receipts[part.Key]
		var err error
		if !known {
			status, err = native.LookupSend(ctx, part.Key)
		}
		if errors.Is(err, linuxnative.ErrToolNotFound) {
			status = linuxnative.ToolStatus{State: "prepared", ThreadID: batch.Target}
			all = false
		} else if err != nil {
			return "", err
		}
		if status.ThreadID != batch.Target || status.SourceID != "" && status.SourceID != part.SourceID {
			return "", errors.New("cross_media_receipt_target_mismatch")
		}
		status = status.WithVoiceReceipts()
		if !known && validCrossReceipt(batch, part, status) {
			// This lookup may be the only observer of a late receipt while the
			// worker is disabled or awaiting a different operation. Commit the
			// verified fact before returning it to the Agent.
			if err = p.manager.settleCrossMediaPart(batch.ID, part.Key, status); err != nil {
				return "", err
			}
		}
		all = all && status.State == "accepted" && status.ServerID != ""
		states = append(states, status)
	}
	state := "queued"
	if all {
		state = "accepted"
	}
	out, e := json.Marshal(map[string]any{"operation_id": id, "state": state, "source_thread_id": batch.Source, "target_thread_id": batch.Target, "parts": states})
	return string(out), e
}

// crossSeedKnown authorizes only read-only reconciliation of a canonical send
// key. Current evidence and send permission are checked separately before any
// new submission, including a retry.
func (m *Manager) crossSeedKnown(seed bot.MediaSeed) bool {
	batch, known := m.crossSeedBatch(seed)
	if !known {
		return false
	}
	for _, part := range batch.Parts {
		if part.Key == seed.Key {
			canonical, queued := part.Media, seed.Media
			// SourceID has a separate durable field and is intentionally json:-
			// in GeneratedMedia. Compare all remaining immutable metadata.
			canonical.SourceID, queued.SourceID = "", ""
			return canonical == queued
		}
	}
	return false
}

func (m *Manager) crossSeedBatch(seed bot.MediaSeed) (crossMediaBatch, bool) {
	batch, e := m.readCrossBatch(seed.Trace)
	if e != nil {
		return batch, false
	}
	found := false
	source := seed.SourceThread
	if source == "" {
		source = seed.OriginThread
	}
	for _, p := range batch.Parts {
		if p.Key == seed.Key && p.SourceID == seed.SourceID && p.Input.Account == seed.Input.Account && p.Input.Thread == seed.Input.Thread && sameCrossOrigin(&p.Input, &seed.Input) && p.OriginThread == seed.OriginThread && p.Requester == seed.Requester && p.OriginTask == seed.OriginTask && source == batch.Source && sameCrossOrigin(p.OriginInput, seed.OriginInput) {
			found = true
			break
		}
	}
	if !found {
		return batch, false
	}
	return batch, true
}

func (m *Manager) crossSeedAllowed(seed bot.MediaSeed) bool {
	batch, known := m.crossSeedBatch(seed)
	if !known || batch.SupersededBy != "" || !batch.CancelledAt.IsZero() {
		return false
	}
	if seed.OriginInput == nil || seed.OriginInput.ReplyContext == nil || seed.OriginInput.Account != batch.Account || seed.OriginInput.Thread != batch.Origin || seed.OriginInput.Sender != batch.Sender {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	access := &permittedTools{manager: m, trace: *seed.OriginInput}
	if err := access.validateCrossMediaOrigin(ctx, seed.OriginInput); err != nil {
		return false
	}
	c := m.agent.config()
	if seed.OriginTask != "" {
		// A cross-session owner task keeps its source separately and executes in
		// its delivery conversation (normally the owner's private chat). Do not
		// require the legacy Thread field to equal the source group here.
		t, ok := m.agent.lookupTask(seed.OriginTask, m.native.Account(), "")
		if !ok || taskSourceThread(t) != seed.OriginThread || t.State == "cancelled" {
			return false
		}
	}
	return c.Enabled && c.CrossConversation && seed.Input.Account == m.native.Account() && contains(c.OwnerIDs, seed.Requester) && c.allowsPrivilegedThread(seed.OriginThread) && permittedRecipient(m.Config().Config, seed.OriginThread) && m.accountThreadKnown(batch.Source) && m.accountThreadKnown(seed.Input.Thread)
}

func (m *Manager) blockCrossRecovery(id, reason string) error {
	// Preserve the original journal for inspection/repair. Remove only this
	// batch's adoption seeds; worker checkpoints also revalidate against it.
	m.mu.Lock()
	if len(m.mediaSeeds) == 0 {
		data, e := os.ReadFile(filepath.Join(m.dir, "media-recovery.json"))
		if e == nil {
			if e = json.Unmarshal(data, &m.mediaSeeds); e != nil {
				m.mu.Unlock()
				return e
			}
		} else if !os.IsNotExist(e) {
			m.mu.Unlock()
			return e
		}
	}
	remaining := []bot.MediaSeed{}
	for _, seed := range m.mediaSeeds {
		if seed.Trace != id {
			remaining = append(remaining, seed)
		}
	}
	if len(remaining) != len(m.mediaSeeds) {
		data, _ := json.Marshal(remaining)
		if e := writePrivate(filepath.Join(m.dir, "media-recovery.json"), data); e != nil {
			m.mu.Unlock()
			return e
		}
		m.mediaSeeds = remaining
	}
	m.mu.Unlock()
	m.record(bot.Record{Event: "cross_media_recovery_blocked", Category: "error", Status: "needs_review", MessageID: id, Reason: reason, ToolCode: reason, Origin: "agent"})
	return nil
}

func (m *Manager) readCrossBatch(id string) (crossMediaBatch, error) {
	var batch crossMediaBatch
	if !validID(id) {
		return batch, errors.New("invalid_cross_media_identity")
	}
	file := filepath.Join(m.dir, "cross-media", id+".json")
	stat, e := os.Lstat(file)
	if errors.Is(e, os.ErrNotExist) {
		return batch, e
	}
	if e != nil || !stat.Mode().IsRegular() {
		return batch, errors.New("cross_media_journal_unavailable")
	}
	data, e := os.ReadFile(file)
	if e != nil || json.Unmarshal(data, &batch) != nil || batch.ID != id {
		return batch, errors.New("invalid_cross_media_journal")
	}
	return batch, validateCrossBatch(batch, m.native.Account())
}

func (m *Manager) crossPartReceipt(id, key string) (linuxnative.ToolStatus, bool) {
	batch, e := m.readCrossBatch(id)
	if e != nil {
		return linuxnative.ToolStatus{}, false
	}
	status, ok := batch.Receipts[key]
	return status, ok
}

func (m *Manager) bindCrossTask(batch crossMediaBatch) error {
	a := m.agent
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.data.Tasks[batch.TaskID]
	if !ok || t.Account != batch.Account || t.Thread != batch.Origin || t.Sender != batch.Sender {
		return errors.New("cross_task_scope_mismatch")
	}
	if !contains(t.PendingOperations, batch.ID) {
		t.PendingOperations = append(append([]string{}, t.PendingOperations...), batch.ID)
		a.data.Tasks[t.ID] = t
		return a.persist()
	}
	return nil
}
func (m *Manager) settleCrossMediaPart(id, key string, status linuxnative.ToolStatus) error {
	if !validID(id) || !validID(key) {
		return errors.New("invalid_cross_receipt_identity")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file := filepath.Join(m.dir, "cross-media", id+".json")
	b, e := os.ReadFile(file)
	if e != nil {
		return e
	}
	var batch crossMediaBatch
	if json.Unmarshal(b, &batch) != nil || batch.Account != m.native.Account() || batch.ID != id {
		return errors.New("cross_batch_account_mismatch")
	}
	if e = validateCrossBatch(batch, m.native.Account()); e != nil {
		return e
	}
	found := false
	for _, part := range batch.Parts {
		if part.Key == key && status.ThreadID == batch.Target && (status.SourceID == "" || status.SourceID == part.SourceID) {
			found = true
		}
	}
	if !found {
		return errors.New("cross_receipt_target_mismatch")
	}
	if status.State != "accepted" || status.ServerID == "" {
		return errors.New("cross_receipt_unverified")
	}
	if batch.Receipts == nil {
		batch.Receipts = map[string]linuxnative.ToolStatus{}
	}
	if old, ok := batch.Receipts[key]; !ok || old.ServerID != status.ServerID {
		batch.Receipts[key] = status
		b, _ = json.Marshal(batch)
		if e = writePrivate(file, b); e != nil {
			return e
		}
	}
	remaining := []bot.MediaSeed{}
	for _, seed := range m.mediaSeeds {
		if seed.Key != key {
			remaining = append(remaining, seed)
		}
	}
	if len(remaining) != len(m.mediaSeeds) {
		b, _ = json.Marshal(remaining)
		if e = writePrivate(filepath.Join(m.dir, "media-recovery.json"), b); e != nil {
			return e
		}
		m.mediaSeeds = remaining
	}
	return m.settleCrossTask(batch)
}

func (m *Manager) settleCrossTask(batch crossMediaBatch) error {
	if batch.TaskID != "" && (crossBatchAccepted(batch) || batch.SupersededBy != "") {
		a := m.agent
		a.mu.Lock()
		t, ok := a.data.Tasks[batch.TaskID]
		var e error
		if ok && t.Account == batch.Account && t.Thread == batch.Origin && t.Sender == batch.Sender {
			pending := []string{}
			for _, op := range t.PendingOperations {
				if op != batch.ID {
					pending = append(pending, op)
				}
			}
			t.PendingOperations = pending
			if batch.SupersededBy == "" && len(pending) == 0 && t.WorkCompleted && t.State == "waiting_delivery" && (t.Delivery == nil || t.Delivery.State == "accepted") {
				t.State = "completed"
			}
			a.data.Tasks[t.ID] = t
			e = a.persist()
		}
		a.mu.Unlock()
		if e != nil {
			return e
		}
		return a.retireSettled()
	}
	return nil
}
