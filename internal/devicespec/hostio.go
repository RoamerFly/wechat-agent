package devicespec

import (
	"context"
	"encoding/json"
	"errors"

	"wechat-bot/internal/mcptools"
)

var ErrNeedsMigration = errors.New("device_broker_needs_migration")

// IOOptions comes from the legacy host's deployment and account settings.
// Plugin requests cannot provide paths, tools, or lease configuration.
type IOOptions struct {
	Python             string
	PackageDirectory   string
	CompatibilityEntry string
	WorkspaceDirectory string
	StateDirectory     string // private to the selected account
	// Leases are shared across accounts inside the trusted deployment.
	GlobalLeaseDirectory string
	// The registered adb path must be a non-reparsed child of a trusted root.
	ADBRoots []string
	Account  string
	Legacy   mcptools.ServerConfig
	Devices  []string
	Actions  []string // permitted side effects; observe is required
}

// IOHost is supplied only by a host that still owns legacy device execution.
type IOHost interface {
	Inspect(context.Context, IOOptions) error
	Grants(context.Context, IOOptions) ([]IOGrant, error)
	Open(context.Context, IOOptions) (IOBroker, error)
	Cleanup() error
}

type IOBroker interface {
	Binding() (string, error)
	Begin(context.Context, string, json.RawMessage) (IOCall, error)
	Close() error
}

type IOCall interface {
	Service(context.Context, string, string, json.RawMessage) (json.RawMessage, error)
	Complete(context.Context, json.RawMessage) (json.RawMessage, error)
	Close() error
}

// IOGrant exposes only the device alias and approved app/action names.
type IOGrant struct {
	DeviceID string   `json:"device_id"`
	Packages []string `json:"packages"`
	Actions  []string `json:"actions"`
}

type ioAuthorityKey struct{}

// WithIOAuthority attaches the original actor and job check to a host call.
func WithIOAuthority(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, ioAuthorityKey{}, check)
}

func CheckIOAuthority(ctx context.Context) error {
	if check, ok := ctx.Value(ioAuthorityKey{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	return nil
}
