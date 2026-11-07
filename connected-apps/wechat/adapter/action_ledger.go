package adapter

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/linuxnative"
)

// ActionProposal preserves the historical action-proposals.json record format.
type ActionProposal struct {
	AppAction *apphost.ActionIntent    `json:"app_action,omitempty"`
	ID        string                   `json:"id"`
	Account   string                   `json:"account"`
	Request   linuxnative.AdminAction  `json:"request"`
	Summary   string                   `json:"summary"`
	State     string                   `json:"state"`
	Created   time.Time                `json:"created"`
	Result    linuxnative.ActionStatus `json:"result"`
}

// ActionLedger owns the private durable WeChat administrative-action records.
type ActionLedger struct {
	mu    sync.Mutex
	path  string
	items map[string]ActionProposal
}

// OpenActionLedger opens the existing per-host or per-worker action ledger.
func OpenActionLedger(dir string) (*ActionLedger, error) {
	ledger := &ActionLedger{path: filepath.Join(dir, "action-proposals.json"), items: map[string]ActionProposal{}}
	b, err := os.ReadFile(ledger.path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil || len(b) > 2<<20 || json.Unmarshal(b, &ledger.items) != nil || ledger.items == nil || len(ledger.items) > 1000 {
		return nil, errors.New("人工操作记录不可用")
	}
	return ledger, nil
}
func (l *ActionLedger) Save(p ActionProposal) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.saveLocked(p)
}

func (l *ActionLedger) saveLocked(p ActionProposal) error {
	if len(l.items) >= 1000 {
		if _, ok := l.items[p.ID]; !ok {
			return errors.New("人工操作记录已达上限")
		}
	}
	old, exists := l.items[p.ID]
	if exists && old.Account != p.Account {
		return apphost.ErrLifecycleConflict
	}
	l.items[p.ID] = cloneActionProposal(p)
	b, err := json.Marshal(l.items)
	if err != nil || len(b) > 2<<20 {
		if exists {
			l.items[p.ID] = old
		} else {
			delete(l.items, p.ID)
		}
		return errors.New("人工操作记录过大")
	}
	if err = hostfile.WritePrivate(l.path, b); err != nil {
		if exists {
			l.items[p.ID] = old
		} else {
			delete(l.items, p.ID)
		}
		return err
	}
	return nil
}
func (l *ActionLedger) Read(id, account string) (ActionProposal, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.items[id]
	if !ok || p.Account != account {
		return ActionProposal{}, false
	}
	return cloneActionProposal(p), true
}
func (l *ActionLedger) List(account string) []ActionProposal {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []ActionProposal{}
	for _, p := range l.items {
		if p.Account == account {
			out = append(out, cloneActionProposal(p))
		}
	}
	return out
}

// SnapshotConnected returns a stable private snapshot of actions that are
// bound to one exact application connection. Its digest covers the complete
// durable rows (including private requests), but is opaque to callers and can
// therefore be retained as handoff evidence without exposing action payloads.
func (l *ActionLedger) SnapshotConnected(ref apphost.ConnectionRef) ([]ActionProposal, string, error) {
	if l == nil || ref.AppID != ID || ref.AccountID == "" || ref.ConnectionID != ConnectionID(ref.AccountID) {
		return nil, "", apphost.ErrNotFound
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	items := make([]ActionProposal, 0, len(l.items))
	for _, p := range l.items {
		if connectedActionMatches(p, ref) {
			items = append(items, cloneActionProposal(p))
		}
	}
	slices.SortFunc(items, func(a, b ActionProposal) int { return strings.Compare(a.ID, b.ID) })
	h := sha256.New()
	for _, p := range items {
		encoded, err := json.Marshal(p)
		if err != nil {
			return nil, "", err
		}
		_, _ = h.Write(encoded)
		_, _ = h.Write([]byte{'\n'})
	}
	return items, fmt.Sprintf("%x", h.Sum(nil)), nil
}

func cloneActionProposal(p ActionProposal) ActionProposal {
	p.Request.Members = slices.Clone(p.Request.Members)
	if p.AppAction != nil {
		intent := *p.AppAction
		p.AppAction = &intent
	}
	return p
}

// FindConnected finds an exact operation or the newest group action within one
// installation and connection, including when the worker is offline.
func (l *ActionLedger) FindConnected(installation, connection, id, group, kind string) (ActionProposal, bool) {
	if l == nil {
		return ActionProposal{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var latest ActionProposal
	for _, p := range l.items {
		ref := apphost.ConnectionRef{AppID: ID, InstallationID: installation, ConnectionID: connection, AccountID: p.Account}
		if !connectedActionMatches(p, ref) ||
			!(IsManagedGroupAction(p.Request.Kind) || id != "" && p.Request.Kind == "recall") ||
			kind != "" && p.Request.Kind != kind {
			continue
		}
		if id != "" && p.ID == id {
			return cloneActionProposal(p), true
		}
		if id == "" && p.Request.ThreadID == group && (latest.ID == "" || p.Created.After(latest.Created)) {
			latest = p
		}
	}
	return cloneActionProposal(latest), latest.ID != ""
}

func connectedActionMatches(p ActionProposal, ref apphost.ConnectionRef) bool {
	return p.AppAction != nil && ref.AppID == ID && ref.AccountID == p.Account &&
		p.AppAction.ConnectionRef == ref && ConnectionID(p.Account) == ref.ConnectionID &&
		p.AppAction.Action == p.Request.Kind && p.AppAction.Target == p.Request.ThreadID && p.AppAction.IdempotencyKey == p.ID
}

// IsManagedGroupAction reports actions that have a connected group receipt.
func IsManagedGroupAction(kind string) bool {
	return kind == "mute" || kind == "rename" || kind == "my_name"
}

// IsFinalActionState reports native terminal action states.
func IsFinalActionState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "rejected" || state == "cancelled"
}

// Receipt projects a connected operation without exposing its private intent.
func (p ActionProposal) Receipt() apphost.ActionReceipt {
	if p.AppAction == nil {
		return apphost.ActionReceipt{}
	}
	out := apphost.ActionReceipt{ConnectionRef: p.AppAction.ConnectionRef, OperationID: p.ID, Action: p.Request.Kind,
		Target: p.Request.ThreadID, State: "submission_unknown", Lookup: "stored"}
	if p.Result.ID != "" {
		ProjectActionReceipt(&out, p.Result)
	} else if p.State == "rejected" {
		out.State = "rejected"
	}
	return out
}

// RejectUnsubmitted preserves the reservation without inventing a native
// operation ID. Call only after the transport has proved it sent nothing.
func (l *ActionLedger) RejectUnsubmitted(p ActionProposal) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, exists := l.items[p.ID]
	if !exists || p.AppAction == nil || !connectedActionMatches(current, p.AppAction.ConnectionRef) ||
		*current.AppAction != *p.AppAction || !sameActionRequest(current.Request, p.Request) || current.Result.ID != "" || current.State != "unknown" {
		return apphost.ErrLifecycleConflict
	}
	current.State = "rejected"
	return l.saveLocked(current)
}

func (p ActionProposal) MatchesReceipt(status linuxnative.ActionStatus) bool {
	if p.Request.Kind == "recall" {
		return MatchRecallReceipt(status, p.Account, p.Request, p.Result.ID)
	}
	return MatchGroupActionReceipt(status, p.Account, p.Request.Kind, p.Request.ThreadID, p.Result.ID)
}

// RecordConnectedReceipt checks identity again under the ledger lock and never
// lets an older or duplicate platform receipt replace authoritative state.
func (l *ActionLedger) RecordConnectedReceipt(p ActionProposal, status linuxnative.ActionStatus) (apphost.ActionReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, exists := l.items[p.ID]
	if !exists || current.Account != p.Account || p.AppAction == nil || !connectedActionMatches(current, p.AppAction.ConnectionRef) ||
		*current.AppAction != *p.AppAction || !sameActionRequest(current.Request, p.Request) {
		return apphost.ActionReceipt{}, apphost.ErrNotFound
	}
	out := current.Receipt()
	if IsFinalActionState(current.State) {
		return out, nil
	}
	if !current.MatchesReceipt(status) {
		out.Lookup = "identity_mismatch"
		return out, nil
	}
	for id, prior := range l.items {
		if id != p.ID && prior.Account == p.Account && prior.Result.ID == status.ID {
			out.Lookup = "identity_mismatch"
			return out, nil
		}
	}
	// Keep the first verified native operation ID and terminal results durable.
	// Concurrent reads cannot replace a terminal receipt with older progress.
	if current.Result.ID == "" || IsFinalActionState(status.State) {
		current.Result, current.State = status, status.State
		if err := l.saveLocked(current); err != nil {
			return out, err
		}
	}
	out.Lookup = "platform"
	ProjectActionReceipt(&out, status)
	return out, nil
}

func sameActionRequest(a, b linuxnative.AdminAction) bool {
	return a.Kind == b.Kind && a.ThreadID == b.ThreadID && a.Text == b.Text &&
		a.Muted == b.Muted && a.MessageID == b.MessageID && a.ServerID == b.ServerID &&
		slices.Equal(a.Members, b.Members)
}
