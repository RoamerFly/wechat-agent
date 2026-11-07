package apphost

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"wechat-bot/internal/pluginhost"
)

var (
	ErrLiveUnavailable = errors.New("application live output or provider is unavailable")
	ErrLiveDenied      = errors.New("application live grant does not allow the requested window")
	ErrLiveConflict    = errors.New("application live session conflicts with a durable operation")
	ErrLiveInput       = errors.New("invalid application live request")
	liveIdentifier     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

const liveRemoteTimeout = 5 * time.Second
const liveStartTimeout = 20 * time.Second

// LiveMediaLocalSource is for a client that captures a source prepared on the
// host (for example a virtual camera). The empty mode is the existing ingest
// contract and keeps old session records valid.
const LiveMediaLocalSource = "local-source"

type LiveGrant struct {
	Allowed     bool
	Version     string
	OpensAt     time.Time
	ClosesAt    time.Time
	MaxDuration time.Duration
}

type LiveStart struct {
	AppID             string `json:"app_id"`
	ConnectionID      string `json:"connection_id"`
	RequestKey        string `json:"request_key"`
	DurationSeconds   int64  `json:"duration_seconds"`
	ProgramArtifactID string `json:"program_artifact_id,omitempty"`
	ProgramSHA256     string `json:"program_sha256,omitempty"`
	ProgramLoop       bool   `json:"program_loop,omitempty"`
}

type LiveTarget struct {
	AppID              string    `json:"app_id"`
	ConnectionID       string    `json:"connection_id"`
	Label              string    `json:"label"`
	OpensAt            time.Time `json:"opens_at"`
	ClosesAt           time.Time `json:"closes_at"`
	MaxDurationSeconds int64     `json:"max_duration_seconds"`
	MediaMode          string    `json:"media_mode,omitempty"`
}

// LiveReference is an immutable, host-issued operation identity. Platform and
// local encoder adapters must use this identity for retries and cleanup.
type LiveReference struct {
	ID                string        `json:"id"`
	Target            ConnectionRef `json:"target"`
	Registration      string        `json:"registration"`
	GrantVersion      string        `json:"grant_version"`
	Deadline          time.Time     `json:"deadline"`
	ProgramArtifactID string        `json:"program_artifact_id,omitempty"`
	ProgramSHA256     string        `json:"program_sha256,omitempty"`
	ProgramLoop       bool          `json:"program_loop,omitempty"`
	MediaMode         string        `json:"media_mode,omitempty"`
}

type LiveReceipt struct {
	Reference LiveReference
	// "ended" is definitive for this operation. A room that is temporarily
	// offline while its original start can still take effect is "unknown".
	State  string
	RoomID string
}

// LiveIngest is transient private routing data returned by an approved
// provider. It is never persisted or included in a user-facing status.
type LiveIngest struct {
	Destination *url.URL                                                `json:"-"`
	DialContext func(context.Context, string, string) (net.Conn, error) `json:"-"`
}

type LiveStartResult struct {
	Receipt LiveReceipt
	Ingest  *LiveIngest `json:"-"`
}

type LiveProvider interface {
	// LiveGrant reads the current host-approved policy for this connection.
	LiveGrant(context.Context, ConnectionRef) (LiveGrant, error)
	// StartLive must honor cancellation before dispatch. Start and Stop use
	// the same operation ID. Query and Stop must never create a broadcast.
	StartLive(context.Context, LiveReference) (LiveStartResult, error)
	QueryLive(context.Context, LiveReference) (LiveReceipt, error)
	StopLive(context.Context, LiveReference) (LiveReceipt, error)
}

// LiveOutput handles only output owned by the exact session. Check is
// read-only and Stop must be idempotent, including when no output was started.
type LiveOutput interface {
	CheckLiveOutput(context.Context, LiveReference) error
	StopLiveOutput(context.Context, LiveReference) error
}

// LiveSourceOutput prepares the exact session's input before the platform may
// start. Prepare succeeds only after the source is ready for client capture.
// Stop must also clean up a partially prepared source or a source whose
// platform start was never attempted.
type LiveSourceOutput interface {
	LiveOutput
	PrepareLiveSource(context.Context, LiveReference) error
}

// liveMediaProvider is optional. Its answer is trusted adapter configuration,
// not a browser/model argument. It must be stable for a given connection.
type liveMediaProvider interface {
	LiveMediaMode(ConnectionRef) string
}

func liveMediaMode(provider LiveProvider, target ConnectionRef) string {
	if p, ok := provider.(liveMediaProvider); ok {
		return p.LiveMediaMode(target)
	}
	return ""
}

// LiveOutputStatus is local transport evidence. Platform acceptance is
// reported independently by LiveReceipt.
type LiveOutputStatus struct {
	State         string `json:"state"`
	MediaMessages uint64 `json:"media_messages"`
}

type liveStreamOutput interface {
	StartLiveOutput(context.Context, LiveReference, LiveIngest) error
	LiveOutputStatus(LiveReference) *LiveOutputStatus
}

type LiveView struct {
	ID                string            `json:"id"`
	State             string            `json:"state"`
	AppID             string            `json:"app_id"`
	ConnectionID      string            `json:"connection_id"`
	RoomID            string            `json:"room_id,omitempty"`
	Deadline          time.Time         `json:"deadline"`
	StopReason        string            `json:"stop_reason,omitempty"`
	LocalStopped      bool              `json:"local_stopped"`
	RemoteEnded       bool              `json:"remote_ended"`
	LocalOutput       *LiveOutputStatus `json:"local_output,omitempty"`
	ProgramArtifactID string            `json:"program_artifact_id,omitempty"`
	ProgramSHA256     string            `json:"program_sha256,omitempty"`
	ProgramLoop       bool              `json:"program_loop,omitempty"`
	MediaMode         string            `json:"media_mode,omitempty"`
}

type liveState struct {
	Reference       LiveReference `json:"reference"`
	Owner           string        `json:"owner"`
	State           string        `json:"state"`
	RoomID          string        `json:"room_id,omitempty"`
	StopReason      string        `json:"stop_reason,omitempty"`
	DispatchClaimed bool          `json:"dispatch_claimed"`
	LocalStopped    bool          `json:"local_stopped"`
	RemoteEnded     bool          `json:"remote_ended"`
}

func (l *LiveController) view(s liveState) LiveView {
	v := LiveView{ID: s.Reference.ID, State: s.State, AppID: s.Reference.Target.AppID,
		ConnectionID: s.Reference.Target.ConnectionID, RoomID: s.RoomID, Deadline: s.Reference.Deadline,
		StopReason: s.StopReason, LocalStopped: s.LocalStopped, RemoteEnded: s.RemoteEnded,
		ProgramArtifactID: s.Reference.ProgramArtifactID, ProgramSHA256: s.Reference.ProgramSHA256, ProgramLoop: s.Reference.ProgramLoop,
		MediaMode: s.Reference.MediaMode}
	if output, ok := l.outputFor(s.Reference.MediaMode).(interface {
		LiveOutputStatus(LiveReference) *LiveOutputStatus
	}); ok {
		v.LocalOutput = output.LiveOutputStatus(s.Reference)
	}
	return v
}

type LiveController struct {
	registry Registry
	store    *pluginhost.Store
	output   LiveOutput
	source   LiveSourceOutput
	now      func() time.Time
	mu       sync.Mutex
	closed   bool
	startID  string
	cancel   context.CancelFunc
	managed  LiveReference
	timer    *time.Timer
	wake     chan struct{}
	remote   sync.Mutex
	local    chan struct{}
}

func NewLiveController(registry Registry, store *pluginhost.Store, output LiveOutput) *LiveController {
	return NewLiveControllerWithSource(registry, store, output, nil)
}

func NewLiveControllerWithSource(registry Registry, store *pluginhost.Store, output LiveOutput, source LiveSourceOutput) *LiveController {
	return &LiveController{registry: registry, store: store, output: output, source: source, now: time.Now,
		wake: make(chan struct{}, 1), local: make(chan struct{}, 1)}
}

func (l *LiveController) outputFor(mode string) LiveOutput {
	switch mode {
	case "":
		return l.output
	case LiveMediaLocalSource:
		return l.source
	default:
		return nil
	}
}

func (l *LiveController) Targets(ctx context.Context) []LiveTarget {
	out := []LiveTarget{}
	if l.output == nil && l.source == nil {
		return out
	}
	for _, app := range l.registry.Applications() {
		entry, _ := l.registry.Lookup(app.ID)
		provider, ok := entry.Provider.(LiveProvider)
		if !ok {
			continue
		}
		connections, err := entry.Provider.Connections(ctx)
		if err != nil {
			continue
		}
		for _, connection := range connections {
			if connection.AccountID == "" || !liveIdentifier.MatchString(connection.ID) {
				continue
			}
			ref := liveConnection(entry, connection)
			mode := liveMediaMode(provider, ref)
			if l.outputFor(mode) == nil {
				continue
			}
			grant, err := provider.LiveGrant(ctx, ref)
			now := l.now()
			if err == nil && grantCovers(grant, now, now.Add(time.Second)) {
				out = append(out, LiveTarget{AppID: ref.AppID, ConnectionID: ref.ConnectionID, Label: connection.Label,
					OpensAt: grant.OpensAt, ClosesAt: grant.ClosesAt, MaxDurationSeconds: int64(grant.MaxDuration / time.Second), MediaMode: mode})
			}
		}
	}
	return out
}

func liveConnection(entry Registration, c Connection) ConnectionRef {
	return ConnectionRef{AppID: entry.ID, InstallationID: entry.InstallationID, ConnectionID: c.ID, AccountID: c.AccountID}
}

func (l *LiveController) resolve(ctx context.Context, appID, connectionID string) (Registration, ConnectionRef, LiveProvider, error) {
	entry, ok := l.registry.Lookup(appID)
	if !ok {
		return Registration{}, ConnectionRef{}, nil, ErrLiveUnavailable
	}
	provider, ok := entry.Provider.(LiveProvider)
	if !ok {
		return Registration{}, ConnectionRef{}, nil, ErrLiveUnavailable
	}
	connections, err := entry.Provider.Connections(ctx)
	if err != nil {
		return Registration{}, ConnectionRef{}, nil, ErrLiveUnavailable
	}
	var target ConnectionRef
	for _, c := range connections {
		if c.ID != connectionID {
			continue
		}
		if c.AccountID == "" || target.ConnectionID != "" {
			return Registration{}, ConnectionRef{}, nil, ErrLiveDenied
		}
		target = liveConnection(entry, c)
	}
	if target.ConnectionID != "" {
		if l.outputFor(liveMediaMode(provider, target)) == nil {
			return Registration{}, ConnectionRef{}, nil, ErrLiveUnavailable
		}
		return entry, target, provider, nil
	}
	return Registration{}, ConnectionRef{}, nil, ErrLiveDenied
}

func validLiveGrant(g LiveGrant) bool {
	return g.Allowed && strings.TrimSpace(g.Version) != "" && !g.OpensAt.IsZero() && g.ClosesAt.After(g.OpensAt) && g.MaxDuration >= time.Second
}

func grantCovers(g LiveGrant, now, deadline time.Time) bool {
	return validLiveGrant(g) && !now.Before(g.OpensAt) && deadline.After(now) && !deadline.After(g.ClosesAt) && deadline.Sub(now) <= g.MaxDuration
}

// validLiveProgram validates the public immutable reference only. The station
// separately proves artifact ownership, source authorization and video bytes.
func validLiveProgram(id, digest string, loop bool) bool {
	if id == "" && digest == "" && !loop {
		return true
	}
	decoded, err := hex.DecodeString(digest)
	return liveIdentifier.MatchString(id) && err == nil && len(decoded) == sha256.Size && digest == strings.ToLower(digest)
}

func decodeLive(r pluginhost.LiveSessionRecord) (liveState, error) {
	var s liveState
	if json.Unmarshal(r.Payload, &s) != nil || s.Reference.ID != r.ID || s.Owner == "" || s.Reference.Target.AccountID == "" ||
		s.Reference.Registration == "" || s.Reference.GrantVersion == "" || s.Reference.Deadline.IsZero() ||
		!validLiveProgram(s.Reference.ProgramArtifactID, s.Reference.ProgramSHA256, s.Reference.ProgramLoop) ||
		(s.Reference.MediaMode != "" && s.Reference.MediaMode != LiveMediaLocalSource) ||
		r.Active == (s.State == "ended") {
		return liveState{}, ErrLiveConflict
	}
	switch s.State {
	case "starting":
		if s.RoomID != "" || s.StopReason != "" || s.LocalStopped || s.RemoteEnded {
			return liveState{}, ErrLiveConflict
		}
	case "live":
		if !s.DispatchClaimed || s.StopReason != "" || !liveIdentifier.MatchString(s.RoomID) || s.LocalStopped || s.RemoteEnded {
			return liveState{}, ErrLiveConflict
		}
	case "stopping", "ended":
		if s.StopReason == "" || (s.RoomID != "" && !liveIdentifier.MatchString(s.RoomID)) ||
			(s.State == "ended" && (!s.LocalStopped || !s.RemoteEnded)) {
			return liveState{}, ErrLiveConflict
		}
	default:
		return liveState{}, ErrLiveConflict
	}
	return s, nil
}

func readLive(r pluginhost.LiveSessionRecord, err error) (liveState, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return liveState{}, ErrNotFound
	}
	if err != nil {
		return liveState{}, err
	}
	return decodeLive(r)
}

func ownLive(s liveState, owner string, err error) (liveState, error) {
	if err != nil {
		return liveState{}, err
	}
	if owner == "" || s.Owner != owner {
		return liveState{}, ErrNotFound
	}
	return s, nil
}

func (l *LiveController) Status(ctx context.Context, owner, id string) (LiveView, error) {
	s, err := readLive(l.store.LiveSession(ctx, id))
	s, err = ownLive(s, owner, err)
	return l.view(s), err
}

func (l *LiveController) Active(ctx context.Context, owner string) (LiveView, error) {
	s, err := readLive(l.store.ActiveLiveSession(ctx))
	s, err = ownLive(s, owner, err)
	return l.view(s), err
}

func (l *LiveController) Start(ctx context.Context, owner string, in LiveStart) (LiveView, error) {
	if owner == "" || !stableID.MatchString(in.AppID) || !liveIdentifier.MatchString(in.ConnectionID) ||
		!liveIdentifier.MatchString(in.RequestKey) || !validLiveProgram(in.ProgramArtifactID, in.ProgramSHA256, in.ProgramLoop) || in.DurationSeconds < 1 || in.DurationSeconds > math.MaxInt64/int64(time.Second) {
		return LiveView{}, ErrLiveInput
	}
	raw, _ := json.Marshal(struct {
		Owner string
		Input LiveStart
	}{owner, in})
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	existing, err := l.store.LiveSessionByRequest(ctx, in.RequestKey)
	if err == nil {
		if existing.Fingerprint != fingerprint {
			return LiveView{}, ErrLiveConflict
		}
		s, decodeErr := decodeLive(existing)
		s, decodeErr = ownLive(s, owner, decodeErr)
		return l.view(s), decodeErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LiveView{}, err
	}
	entry, target, provider, err := l.resolve(ctx, in.AppID, in.ConnectionID)
	if err != nil {
		return LiveView{}, err
	}
	now := l.now().UTC()
	deadline := now.Add(time.Duration(in.DurationSeconds) * time.Second)
	grant, err := provider.LiveGrant(ctx, target)
	if err != nil || !grantCovers(grant, now, deadline) {
		return LiveView{}, ErrLiveDenied
	}
	s := liveState{Owner: owner, State: "starting", Reference: LiveReference{ID: uuid.NewString(), Target: target,
		Registration: entry.Registration, GrantVersion: grant.Version, Deadline: deadline,
		ProgramArtifactID: in.ProgramArtifactID, ProgramSHA256: in.ProgramSHA256, ProgramLoop: in.ProgramLoop,
		MediaMode: liveMediaMode(provider, target)}}
	initial := s
	ref := initial.Reference
	raw, _ = json.Marshal(s)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return LiveView{}, ErrLiveUnavailable
	}
	r, created, err := l.store.CreateLiveSession(ctx, pluginhost.LiveSessionRecord{
		ID: s.Reference.ID, RequestKey: in.RequestKey, Fingerprint: fingerprint, Active: true, Payload: raw,
	})
	if err != nil || !created {
		l.mu.Unlock()
		if errors.Is(err, pluginhost.ErrLiveSessionBusy) || errors.Is(err, pluginhost.ErrRequestConflict) {
			return LiveView{}, ErrLiveConflict
		}
		s, err = readLive(r, err)
		s, err = ownLive(s, owner, err)
		return l.view(s), err
	}
	startCtx, cancel := context.WithTimeout(ctx, min(liveStartTimeout, deadline.Sub(l.now())))
	l.startID, l.cancel = s.Reference.ID, cancel
	l.managed = s.Reference
	if l.timer != nil {
		l.timer.Stop()
	}
	l.timer = time.AfterFunc(deadline.Sub(l.now()), func() {
		ctx, done := context.WithTimeout(context.Background(), 2*liveRemoteTimeout)
		defer done()
		_, _ = l.stopSession(ctx, initial, "deadline")
	})
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
	defer func() {
		l.mu.Lock()
		if l.startID == ref.ID {
			l.startID, l.cancel = "", nil
		}
		l.mu.Unlock()
	}()

	reason, receipt := l.dispatch(startCtx, s, provider)
	cancel()

	// The browser can disappear after the platform accepted the request.
	// Settle the same durable operation independently of that connection.
	recovery, done := context.WithTimeout(context.WithoutCancel(ctx), 2*liveRemoteTimeout)
	defer done()
	if reason == "" && receipt.State == "live" {
		reason = l.check(recovery, s.Reference)
	}
	if reason != "" {
		s, err = l.requestStop(recovery, s, reason)
	}
	if err == nil && receipt.State != "" {
		s, err = l.applyLiveReceipt(recovery, s, receipt)
	}
	if err == nil && s.State == "stopping" {
		s, err = l.finishStop(recovery, s, false)
	}
	if err != nil {
		l.stopOutput(context.WithoutCancel(recovery), ref, false)
	}
	return l.view(s), err
}
