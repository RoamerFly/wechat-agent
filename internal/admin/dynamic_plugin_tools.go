package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	catalog "wechat-bot/plugins"
)

// Dynamic packages are deliberately limited to synchronously verifiable
// observations and computations. Packages which create media, send messages,
// manage devices, or own asynchronous work still need their dedicated host
// adapter so that their output lineage and cancellation rules can be checked.
func dynamicPluginCapability(cap pluginhost.Capability) bool {
	// The generic runner has no universal broker for device, media, workspace or
	// model permissions. A declaration for one of those resources must remain on
	// its dedicated adapter until the host can verify its actual effect. Network
	// is admitted separately below only for one fixed public HTTP route.
	return cap.Discoverable && cap.Implementation == "ready" && !cap.Async && len(cap.Permissions) == 0 && (cap.SideEffect == "" || cap.SideEffect == "read" || cap.SideEffect == "compute")
}

// dynamicPublicHTTPRoute is the only network protocol available to a future
// package without a dedicated adapter. It is one fixed public HTTPS endpoint,
// uses no configured endpoint, credential, headers or referer, and is bound to
// exactly one declared capability. The plugin can ask the host to post only
// the original authorized arguments to that exact route.
func dynamicPublicHTTPRoute(pack pluginhost.Package, cap pluginhost.Capability) (pluginhost.HTTPRoute, bool) {
	if !cap.Discoverable || cap.Implementation != "ready" || cap.Async || len(cap.Permissions) != 1 || cap.Permissions[0] != "network" || (cap.SideEffect != "" && cap.SideEffect != "read" && cap.SideEffect != "compute") {
		return pluginhost.HTTPRoute{}, false
	}
	var selected *pluginhost.HTTPRoute
	for _, route := range pack.HTTPRoutes {
		if route.Permission != "" && route.Permission != "network" || route.Endpoint == "" || route.EndpointConfig != "" || route.Credential != "" || route.CredentialHeader != "" || route.CredentialPrefix != "" || route.RefererPathPrefix != "" || len(route.Headers) != 0 || len(route.Capabilities) != 1 || route.Capabilities[0] != cap.ID {
			continue
		}
		if selected != nil {
			return pluginhost.HTTPRoute{}, false
		}
		copy := route
		selected = &copy
	}
	if selected == nil {
		return pluginhost.HTTPRoute{}, false
	}
	return *selected, true
}

func dynamicPluginCapabilityForPackage(pack pluginhost.Package, cap pluginhost.Capability) bool {
	if dynamicPluginCapability(cap) {
		return true
	}
	_, ok := dynamicPublicHTTPRoute(pack, cap)
	return ok
}

func builtInServicePlugin(id string) bool {
	// Any catalog package has a reviewed adapter or reserved compatibility
	// route. Never let an official package fall through merely because a new
	// catalog item was added after this source file was written.
	_, err := catalog.ContractFor(id)
	return err == nil
}

// Official packages keep their purpose-built adapters. The generic bridge must
// not expose their internal capabilities, but discovery should still tell the
// Agent which already-authorized host tools implement that package. This uses
// the individual adapter lists rather than p.List/localTools so service plugin
// discovery cannot recurse back into itself.
func (p *permittedTools) dedicatedPluginToolNames(ctx context.Context, id string) []string {
	// Windows production retains the package's historical task records but must
	// not advertise a legacy device action as an available Agent capability.
	if id == mobileDevicePluginID && p != nil && p.manager != nil && p.manager.mobileRetired {
		return nil
	}
	var tools []mcptools.Tool
	switch id {
	case "music":
		tools = append(tools, p.yueTools()...)
	case "speech":
		tools = append(tools, p.speechTools()...)
		tools = append(tools, p.voiceTools()...)
	case "media-understanding":
		tools = append(tools, p.videoTools()...)
	case "mobile-control":
		tools = append(tools, p.mobileTools()...)
	case "project-development":
		tools = append(tools, p.projectPluginTools(ctx)...)
		tools = append(tools, p.projectTools()...)
	case "document-workshop":
		tools = append(tools, p.documentTools()...)
	case "external-tools":
		tools = append(tools, p.installedExternalHTTPTools(ctx)...)
		tools = append(tools, p.installedExternalJobTools(ctx)...)
		tools = append(tools, p.installedExternalPollTools(ctx)...)
	default:
		return nil
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Name != "" && !seen[tool.Name] {
			seen[tool.Name] = true
			names = append(names, tool.Name)
		}
	}
	return names
}

// Dynamic packages can receive account-private configuration and declared
// permissions. Until a package declares a narrower, reviewable conversation
// grant, only the configured owner may invoke it. The invocation is still
// bound to the current account and conversation below.
func (p *permittedTools) dynamicPluginAllowed() bool {
	return p.privateSurfOrigin == nil && !p.trace.Surf && p.privileged()
}

func (p *permittedTools) dynamicPluginCapabilities(ctx context.Context, id string) ([]pluginhost.Capability, error) {
	if id == mobileDevicePluginID && p != nil && p.manager != nil && p.manager.mobileRetired {
		return nil, nil
	}
	if !p.dynamicPluginAllowed() || builtInServicePlugin(id) {
		return nil, nil
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return nil, err
	}
	installed, err := store.Installed(ctx, id)
	if err != nil || !installed.Enabled || installed.Removed {
		return nil, err
	}
	if installed.Package.Runtime.Transport != "mcp-stdio" {
		// No generic remote transport/credential proxy exists. Do not advertise a
		// package merely because its declaration looks callable.
		return nil, nil
	}
	if _, err = store.ConfigurationForExecution(ctx, id); err != nil {
		return nil, nil
	}
	caps, err := store.Capabilities(ctx, id)
	if err != nil {
		return nil, err
	}
	accepted := make([]pluginhost.Capability, 0, len(caps))
	for _, cap := range caps {
		invocation := pluginhost.Invocation{Account: p.trace.Account, Thread: p.trace.Thread, Requester: p.serviceRequester(), Purpose: "conversation service discovery", RequestKey: "service-discovery"}
		grant := pluginhost.Grant{Account: store.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}
		if dynamicPluginCapabilityForPackage(installed.Package, cap) && pluginhost.Authorize(installed.Package, grant, invocation, cap.Permissions) == nil {
			accepted = append(accepted, cap)
		}
	}
	return accepted, nil
}

// dynamicPluginPolicy supplies the common provenance, grant and result bounds
// for packages installed after the host was built. It never gives plugin code a
// delivery channel, media staging authority, host paths, or a way to expand its
// installed permission set.
type dynamicPluginPolicy struct {
	p          *permittedTools
	plugin     string
	capability string
	invocation pluginhost.Invocation
	post       pluginruntime.RouteTransport
	routeUsed  atomic.Bool
}

func (p *permittedTools) newDynamicPluginPolicy(ctx context.Context, plugin, capability string, invocation pluginhost.Invocation) (*dynamicPluginPolicy, error) {
	policy := &dynamicPluginPolicy{p: p, plugin: plugin, capability: capability, invocation: invocation}
	if err := policy.Check(ctx, pluginhost.Job{Plugin: plugin, Tool: capability, Context: invocation}, pluginhost.Capability{}); err != nil {
		return nil, err
	}
	return policy, nil
}

func (d *dynamicPluginPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) error {
	p := d.p
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.dynamicPluginAllowed() || job.Plugin != d.plugin || job.Tool != d.capability || !sameDynamicInvocation(job.Context, d.invocation) {
		return errors.New("dynamic plugin authorization changed")
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return err
	}
	installed, err := store.Installed(ctx, d.plugin)
	if err != nil || !installed.Enabled || installed.Removed || builtInServicePlugin(d.plugin) {
		return errors.New("dynamic plugin is not enabled")
	}
	if job.PackageDigest != "" && (job.PackageDigest != installed.Digest || job.Version != installed.Package.Version) {
		return errors.New("dynamic plugin package changed")
	}
	configuration, err := store.ConfigurationForExecution(ctx, d.plugin)
	if err != nil || job.ConfigRevision != "" && job.ConfigRevision != strconv.Itoa(configuration.Revision) {
		return errors.New("dynamic plugin configuration changed")
	}
	caps, err := d.p.dynamicPluginCapabilities(ctx, d.plugin)
	if err != nil {
		return err
	}
	var declared pluginhost.Capability
	for _, item := range caps {
		if item.ID == d.capability {
			declared = item
			break
		}
	}
	if declared.ID == "" || cap.ID != "" && cap.ID != declared.ID {
		return errors.New("dynamic plugin capability unavailable")
	}
	if err := pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: store.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, job.Context, declared.Permissions); err != nil {
		return errors.New("dynamic plugin permission revoked")
	}
	if job.ID != "" {
		current, err := store.Job(ctx, job.ID)
		if err != nil || current.Revision != job.Revision || current.CancelRequested || current.State == "cancelled" {
			return errors.New("dynamic plugin task changed or cancelled")
		}
	}
	if job.Context.Account != p.trace.Account || job.Context.Thread != p.trace.Thread || job.Context.Requester != p.serviceRequester() || job.Context.Purpose != "conversation service request" || job.Context.Authorization == "" || job.Context.Authorization != p.serviceAuthorization() || !p.manager.agentTaskActive(p.trace) {
		return errors.New("dynamic plugin request authorization changed")
	}
	epoch, err := p.serviceEvidenceEpoch(ctx)
	if err != nil || job.Context.EvidenceEpoch == "" || job.Context.EvidenceEpoch != epoch {
		return errors.New("dynamic plugin evidence changed")
	}
	for id, revision := range job.Context.SourceVersions {
		event, err := p.manager.memory.ReadEvent(ctx, []string{companion.ScopeID("wechat", job.Context.Account, job.Context.Thread)}, id)
		if err != nil || event.Revision != revision {
			return errors.New("dynamic plugin source unavailable or changed")
		}
	}
	return nil
}

func (d *dynamicPluginPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	if err := d.Check(ctx, job, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if len(raw) == 0 || len(raw) > 256<<10 {
		return pluginruntime.VerifiedResult{}, errors.New("dynamic plugin result size invalid")
	}
	result, err := dynamicPluginResultPayload(raw)
	if err != nil {
		return pluginruntime.VerifiedResult{}, errors.New("dynamic plugin result must be one object")
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(result)))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil || decoder.Decode(new(any)) != io.EOF {
		return pluginruntime.VerifiedResult{}, errors.New("dynamic plugin result must be one object")
	}
	if dynamicResultClaimsHostDelivery(object) {
		return pluginruntime.VerifiedResult{}, errors.New("dynamic plugin result cannot claim host delivery or media")
	}
	declared, err := d.declaredCapability(ctx)
	if err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var schema inputSchema
	if json.Unmarshal(declared.OutputSchema, &schema) != nil {
		return pluginruntime.VerifiedResult{}, errors.New("dynamic plugin output schema unavailable")
	}
	if _, err := normalizeSchemaValue(object, schema, "$"); err != nil {
		return pluginruntime.VerifiedResult{}, errors.New("dynamic plugin result does not match declared schema")
	}
	return pluginruntime.VerifiedResult{Result: append(json.RawMessage(nil), result...)}, nil
}

// Service exposes the narrowly declared public HTTPS route to the running
// plugin. It is intentionally not a general network bridge: the plugin cannot
// choose a URL, headers, referer, credential, request body or retry key.
func (d *dynamicPluginPolicy) Service(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	if err := d.Check(ctx, job, cap); err != nil {
		return nil, err
	}
	if method != "wechat/http_route" {
		return nil, errors.New("dynamic plugin host service unavailable")
	}
	declared, err := d.declaredCapability(ctx)
	if err != nil {
		return nil, err
	}
	store, err := d.p.manager.packageStore()
	if err != nil {
		return nil, err
	}
	installed, err := store.Installed(ctx, d.plugin)
	if err != nil {
		return nil, err
	}
	route, allowed := dynamicPublicHTTPRoute(installed.Package, declared)
	if !allowed || cap.ID != declared.ID {
		return nil, errors.New("dynamic plugin public route unavailable")
	}
	var request struct {
		Route    string          `json:"route"`
		Body     json.RawMessage `json:"body"`
		MaxBytes int64           `json:"max_bytes"`
		Timeout  int64           `json:"timeout_seconds"`
	}
	if strictServicePluginJSON(raw, &request) != nil || request.Route != route.ID || !sameJSON(request.Body, job.Arguments) {
		return nil, errors.New("dynamic plugin public route binding rejected")
	}
	post := d.post
	if post == nil {
		post = mcptools.PostPluginRoute
	}
	return pluginruntime.ProxyHTTPRoute(ctx, store, job, declared, raw, func(routeID string, body json.RawMessage) error {
		if routeID != route.ID || !sameJSON(body, job.Arguments) {
			return errors.New("dynamic plugin public route binding rejected")
		}
		return nil
	}, func(call context.Context, endpoint string, headers map[string]string, body json.RawMessage, maxBytes int64, timeout time.Duration) (json.RawMessage, error) {
		if endpoint != route.Endpoint || len(headers) != 0 || !sameJSON(body, job.Arguments) {
			return nil, errors.New("dynamic plugin public route changed")
		}
		// A plugin may not turn a timed-out/unknown public submission into a
		// second submission in the same durable host job. The reservation is
		// intentionally made immediately before transport, after every route and
		// authorization check has succeeded.
		if d.routeUsed.Swap(true) {
			return nil, errors.New("dynamic plugin public route already submitted")
		}
		return post(call, endpoint, headers, body, maxBytes, timeout)
	})
}

// mcp-stdio returns an MCP tools/call result envelope. The host persists only
// one verified JSON text item, never an arbitrary content array or an error
// envelope. Test executors may return the object directly, which is treated as
// the same normalized result.
func dynamicPluginResultPayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errors.New("invalid dynamic result")
	}
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError *bool `json:"isError"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, errors.New("invalid dynamic result")
	}
	if envelope.IsError == nil && envelope.Content == nil {
		return append(json.RawMessage(nil), raw...), nil
	}
	if envelope.IsError == nil || *envelope.IsError || len(envelope.Content) != 1 || envelope.Content[0].Type != "text" || !json.Valid([]byte(envelope.Content[0].Text)) {
		return nil, errors.New("invalid dynamic MCP result")
	}
	return json.RawMessage(append([]byte(nil), envelope.Content[0].Text...)), nil
}

func (d *dynamicPluginPolicy) declaredCapability(ctx context.Context) (pluginhost.Capability, error) {
	caps, err := d.p.dynamicPluginCapabilities(ctx, d.plugin)
	if err != nil {
		return pluginhost.Capability{}, err
	}
	for _, cap := range caps {
		if cap.ID == d.capability {
			return cap, nil
		}
	}
	return pluginhost.Capability{}, errors.New("dynamic plugin capability unavailable")
}

func dynamicResultClaimsHostDelivery(object map[string]any) bool {
	for key, value := range object {
		switch strings.ToLower(strings.ReplaceAll(key, "-", "_")) {
		case "asset", "asset_id", "asset_ids", "media", "media_id", "media_ids", "message", "message_id", "message_ids", "delivery", "delivery_id", "receipt", "wechat", "send", "send_result", "upload", "file_path", "path", "native_job_id":
			return true
		}
		switch nested := value.(type) {
		case map[string]any:
			if dynamicResultClaimsHostDelivery(nested) {
				return true
			}
		case []any:
			for _, item := range nested {
				if nestedObject, ok := item.(map[string]any); ok && dynamicResultClaimsHostDelivery(nestedObject) {
					return true
				}
			}
		}
	}
	return false
}

// Invocation includes a source-version map, so it is not comparable in Go.
// JSON encoding sorts map keys, which makes this a stable identity check while
// retaining every provenance field instead of silently comparing a subset.
func sameDynamicInvocation(left, right pluginhost.Invocation) bool {
	l, leftErr := json.Marshal(left)
	r, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(l) == string(r)
}

var _ pluginruntime.CallPolicy = (*dynamicPluginPolicy)(nil)
