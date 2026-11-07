package pluginhost

import (
	"encoding/json"
	"errors"
	"strings"
)

// Invocation is populated by the authenticated host, never deserialized from
// tool arguments. Tokens and arbitrary filesystem paths do not belong here.
type Invocation struct {
	Account        string         `json:"account"`
	Thread         string         `json:"thread"`
	Requester      string         `json:"requester"`
	Purpose        string         `json:"purpose"`
	RequestKey     string         `json:"request_key"`
	SourceVersions map[string]int `json:"source_versions"`
	EvidenceEpoch  string         `json:"evidence_epoch,omitempty"`
	Authorization  string         `json:"authorization,omitempty"`
	// OriginSourceSender and RequestThread are audit-only provenance for an
	// explicitly authorised cross-message read. They never replace Requester,
	// which remains the current actor that requested the operation.
	OriginSourceSender string `json:"origin_source_sender,omitempty"`
	RequestThread      string `json:"request_thread,omitempty"`
}
type Grant struct {
	Account     string
	Plugin      string
	Version     string
	Permissions []string
}

// Authorize is only the static permission gate. It does not launch a process
// or assert that platform isolation, dependency health or source validity pass.
func Authorize(p Package, g Grant, c Invocation, required []string) error {
	if p.Status != "ready" {
		return errors.New("plugin implementation unavailable")
	}
	if c.Account == "" || c.Thread == "" || c.Requester == "" || strings.TrimSpace(c.Purpose) == "" || c.RequestKey == "" {
		return errors.New("host invocation context incomplete")
	}
	if g.Account != c.Account || g.Plugin != p.ID || g.Version != p.Version {
		return errors.New("plugin grant scope or version mismatch")
	}
	declared := map[string]bool{}
	approved := map[string]bool{}
	for _, v := range p.Permissions {
		declared[v] = true
	}
	for _, v := range g.Permissions {
		approved[v] = true
	}
	for _, v := range required {
		if !declared[v] || !approved[v] {
			return errors.New("plugin permission not granted")
		}
	}
	return nil
}

type Job struct {
	ID              string          `json:"id"`
	Plugin          string          `json:"plugin"`
	Version         string          `json:"version"`
	ConfigRevision  string          `json:"config_revision"`
	Context         Invocation      `json:"context"`
	RemoteID        string          `json:"remote_id,omitempty"`
	State           string          `json:"state"`
	Artifacts       []string        `json:"artifacts,omitempty"`
	PackageDigest   string          `json:"package_digest"`
	Tool            string          `json:"tool"`
	Arguments       json.RawMessage `json:"arguments"`
	Submission      json.RawMessage `json:"submission,omitempty"`
	Revision        int             `json:"revision"`
	CancelRequested bool            `json:"cancel_requested"`
	Created         string          `json:"created"`
	Updated         string          `json:"updated"`
	Result          json.RawMessage `json:"result,omitempty"`
	Diagnostic      *JobDiagnostic  `json:"diagnostic,omitempty"`
}

// JobFailureStage is chosen by trusted host control flow, not parsed from a
// plugin response or arbitrary error string.
type JobFailureStage string

const (
	JobFailureDispatch   JobFailureStage = "dispatch"
	JobFailureAuthority  JobFailureStage = "authority"
	JobFailureValidation JobFailureStage = "validation"
	JobFailureResultMode JobFailureStage = "result_mode"
)

// JobDiagnostic contains only fixed host-owned labels. It is independent from
// Result: an uncertain call still has no validated provider result to expose.
type JobDiagnostic struct {
	Stage JobFailureStage `json:"stage"`
	Code  string          `json:"code"`
}

// Recovery never infers successful delivery or retries an unknown submission.
func (j Job) Recovery() string {
	switch j.State {
	case "pending":
		return "claim"
	case "completed", "failed", "cancelled":
		return "terminal"
	}
	if j.RemoteID != "" {
		return "query"
	}
	return "needs_review"
}
