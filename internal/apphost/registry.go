// Package apphost describes the applications and connections registered by a
// trusted station host. App packages and conversation tools cannot register
// themselves through this interface.
package apphost

import (
	"context"
	"errors"
	"regexp"
	"sort"
)

var (
	ErrNotFound          = errors.New("application or connection not found")
	ErrUnavailable       = errors.New("application registry unavailable")
	ErrLifecycleConflict = errors.New("application lifecycle transition unavailable now")
	// ErrLifecycleCutover means an imported application connection has not
	// supplied (or no longer satisfies) its local old-host stop attestation.
	// It is distinct from a transient lifecycle conflict: retrying cannot safely
	// activate the imported worker until the local evidence is repaired.
	ErrLifecycleCutover        = errors.New("application lifecycle cutover attestation required")
	ErrInvalidConnectionPolicy = errors.New("invalid connection reply policy")
	ErrLifecycleNotReady       = errors.New("application requires configuration before start")
	stableID                   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

type Application struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	InstallationID string `json:"installation_id"`
	Registration   string `json:"registration"`
	AccountSetup   bool   `json:"account_setup,omitempty"`
}

type Connection struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	Label         string `json:"label,omitempty"`
	Primary       bool   `json:"primary,omitempty"`
	Configuration string `json:"configuration"`
}

// ConnectionGroupReplySettings configures how an already-authorized group may
// start a reply. It is optional so older callers that do not know about reply
// settings cannot accidentally reset a group's existing settings.
type ConnectionGroupReplySettings struct {
	Mode                    string `json:"mode,omitempty"`
	Prefix                  string `json:"prefix,omitempty"`
	Persona                 string `json:"persona,omitempty"`
	ForceReplyWhenMentioned bool   `json:"force_reply_when_mentioned,omitempty"`
}

// ConnectionGroupRule configures one already-authorized conversation.
type ConnectionGroupRule struct {
	ThreadID        string `json:"thread_id"`
	Note            string `json:"note"`
	SummarizeShares bool   `json:"summarize_shares"`
	// ReplySettings is deliberately optional. A missing value preserves the
	// stored reply settings, while a present value explicitly replaces them.
	ReplySettings *ConnectionGroupReplySettings `json:"reply_settings,omitempty"`
}

// ConfiguredPolicy is the existing account policy, without owner/recipient IDs
// or credentials. A caller still needs request- and target-specific admission.
type ConfiguredPolicy struct {
	WorkerEnabled       bool                  `json:"worker_enabled"`
	ConversationEnabled bool                  `json:"conversation_enabled"`
	ToolsEnabled        bool                  `json:"tools_enabled"`
	Grants              []string              `json:"grants"`
	ReplyThreads        []string              `json:"reply_threads,omitempty"`
	AttachmentThreads   []string              `json:"attachment_threads,omitempty"`
	AutoObserveImages   bool                  `json:"auto_observe_images,omitempty"`
	AutoObserveVideos   bool                  `json:"auto_observe_videos,omitempty"`
	MediaReplyThreads   []string              `json:"media_reply_threads,omitempty"`
	HistoryThreads      []string              `json:"history_threads,omitempty"`
	ContactRead         bool                  `json:"contact_read,omitempty"`
	GroupMuteThreads    []string              `json:"group_mute_threads,omitempty"`
	GroupRules          []ConnectionGroupRule `json:"group_rules,omitempty"`
}

type ConnectionStatus struct {
	Connection
	Login             string            `json:"login"`
	Runtime           string            `json:"runtime"`
	Worker            string            `json:"worker"`
	SubmissionsPaused bool              `json:"submissions_paused"`
	Policy            *ConfiguredPolicy `json:"configured_policy"`
	CapabilityCheck   string            `json:"capability_check"`
	SupportedActions  []string          `json:"supported_actions"`
	LegacyImport      string            `json:"legacy_import,omitempty"`
}

type ConnectionLifecycle struct {
	ConnectionRef
	Worker            string `json:"worker"`
	WorkerEnabled     bool   `json:"worker_enabled"`
	SubmissionsPaused bool   `json:"submissions_paused"`
}

type LifecycleController interface {
	SetConnectionRunning(ctx context.Context, connectionID string, running bool) (ConnectionLifecycle, error)
}

// ConnectionPolicyWriter saves explicitly selected conversation capabilities.
type ConnectionPolicyWriter interface {
	SetConnectionPolicy(ctx context.Context, connectionID string, policy ConnectionPolicyInput) (ConnectionStatus, error)
}

type ConnectionPolicyInput struct {
	ReplyThreads []string
	// A nil list leaves the old attachment grant on still-allowed threads.
	AttachmentThreads *[]string
	AutoObserveImages *bool
	AutoObserveVideos *bool
	MediaReplyThreads *[]string
	HistoryThreads    *[]string
	ContactRead       *bool
	GroupMuteThreads  *[]string
	GroupRules        *[]ConnectionGroupRule
}

type Provider interface {
	Connections(context.Context) ([]Connection, error)
	Inspect(context.Context, string) (ConnectionStatus, error)
}

type Registration struct {
	Application
	Provider Provider
}

// Registry is immutable after construction. Its zero value represents a host
// with no registered applications.
type Registry struct {
	apps map[string]Registration
}

func New(registrations ...Registration) (Registry, error) {
	r := Registry{apps: make(map[string]Registration, len(registrations))}
	installations := map[string]bool{}
	for _, entry := range registrations {
		if !stableID.MatchString(entry.ID) || !stableID.MatchString(entry.InstallationID) || entry.Name == "" || entry.Registration == "" || entry.Provider == nil {
			return Registry{}, errors.New("invalid application registration")
		}
		if _, exists := r.apps[entry.ID]; exists || installations[entry.InstallationID] {
			return Registry{}, errors.New("duplicate application or installation")
		}
		_, entry.AccountSetup = entry.Provider.(AccountSetup)
		r.apps[entry.ID] = entry
		installations[entry.InstallationID] = true
	}
	return r, nil
}

func (r Registry) Applications() []Application {
	result := make([]Application, 0, len(r.apps))
	for _, entry := range r.apps {
		result = append(result, entry.Application)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r Registry) Lookup(id string) (Registration, bool) {
	entry, exists := r.apps[id]
	return entry, exists
}
