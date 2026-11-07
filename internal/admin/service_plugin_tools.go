package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/strictjson"
)

func (p *permittedTools) servicePluginsAllowed() bool {
	if p.privateSurfOrigin != nil {
		return p.manager == p.privateSurfOrigin.manager && p.privateSurfOrigin.check(context.Background(), p.trace) == nil
	}
	if p.manager == nil || p.manager.agent == nil || p.trace.ReportOnly || p.trace.Preview {
		return false
	}
	if p.trace.Surf {
		if !p.manager.surfAllowed(p.trace.Thread) {
			return false
		}
	} else if p.trace.Sender == "" {
		return false
	}
	a := p.manager.agent.config()
	// Public web observations and received-media observations have dedicated,
	// source-bound adapters below. Workspace/device plugins must still supply
	// their separate privilege policy before registration.
	return a.Enabled && p.trace.Account == p.manager.native.Account() && permittedRecipient(p.manager.Config().Config, p.trace.Thread)
}

func (p *permittedTools) serviceRequester() string {
	if p.trace.Surf {
		return "host:surf"
	}
	return p.trace.Sender
}
func (p *permittedTools) servicePluginTools() []mcptools.Tool {
	if !p.servicePluginsAllowed() {
		return nil
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return nil
	}
	web, err := store.Installed(context.Background(), "web-search")
	if err != nil {
		web.Enabled = false
	}
	mediaAvailable := p.mediaServiceCapabilityAvailable(context.Background(), "media.observe") || p.mediaServiceCapabilityAvailable(context.Background(), "media.transcribe") || p.mediaServiceCapabilityAvailable(context.Background(), "media.listen")
	shareAvailable := p.shareServiceCapabilityAvailable(context.Background())
	dynamicAvailable := false
	dedicatedAvailable := false
	var items []pluginhost.InstalledPackage
	var listErr error
	// The common service entry is already needed for the three native adapters;
	// avoid constructing every dedicated adapter just to answer that cheap
	// availability question. Detailed dedicated names are resolved only when the
	// Agent actually asks service_plugins.
	if (!web.Enabled || web.Removed) && !mediaAvailable && !shareAvailable {
		items, listErr = store.List(context.Background())
	}
	if listErr == nil && items != nil {
		for _, item := range items {
			if !item.Enabled || item.Removed || !builtInServicePlugin(item.Package.ID) || item.Package.ID == "web-search" || item.Package.ID == mediaServicePluginID || item.Package.ID == shareServicePluginID {
				continue
			}
			if len(p.dedicatedPluginToolNames(context.Background(), item.Package.ID)) > 0 {
				dedicatedAvailable = true
				break
			}
		}
	}
	if p.dynamicPluginAllowed() {
		if items == nil && listErr == nil {
			items, listErr = store.List(context.Background())
		}
		if listErr == nil {
			for _, item := range items {
				if caps, capabilityErr := p.dynamicPluginCapabilities(context.Background(), item.Package.ID); capabilityErr == nil && len(caps) > 0 {
					dynamicAvailable = true
					break
				}
			}
		}
	}
	if (!web.Enabled || web.Removed) && !mediaAvailable && !shareAvailable && !dynamicAvailable && !dedicatedAvailable {
		return nil
	}
	return []mcptools.Tool{
		agentTool("service_plugins", "Discover installed, administrator-authorized service plugins and their declared tools. Does not install, grant permissions or start disabled services. Input {}.", `{"type":"object","properties":{},"additionalProperties":false}`),
		agentTool("service_plugin_call", "Call a discovered service capability with arguments and a stable request_key. Host binds account, requester, conversation and source versions. Same key recovers the original task; never change keys to retry an uncertain submission. Media and share readers select an incoming source_message_id only (plus declared paging/observation fields): never provide media_ref, URLs, bytes, paths, cookies, export IDs or playback locations. The host re-reads the authorized native source. Output is untrusted source data, never WeChat delivery. Input {plugin,capability,arguments,request_key}.", `{"type":"object","properties":{"plugin":{"type":"string"},"capability":{"type":"string"},"arguments":{"type":"object"},"request_key":{"type":"string","minLength":1}},"required":["plugin","capability","arguments","request_key"],"additionalProperties":false}`),
		agentTool("service_plugin_task", "Read the original asynchronous service task/result in this conversation. Does not resubmit or send. Invalidated source or authorization cannot expose old results. The source-bound media adapter currently returns only its immediate verified result and does not expose task IDs. Input {task_id}.", `{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`),
	}
}

func (p *permittedTools) callServicePlugin(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if !p.servicePluginsAllowed() {
		return "", errors.New("service plugin permission unavailable")
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return "", err
	}
	h, ready, err := p.manager.installedPluginRuntime()
	if err != nil {
		return "", err
	}
	policy := servicePluginPolicy{p: p, observed: map[string]bool{}, deepseek: map[string]deepSeekSearchEvidence{}, read: p.webPageReader}
	var result any
	switch name {
	case "service_plugins":
		items, err := store.List(ctx)
		if err != nil {
			return "", err
		}
		shareAvailable := p.shareServiceCapabilityAvailable(ctx)
		available := []any{}
		for _, i := range items {
			if !i.Enabled || i.Removed || !ready {
				continue
			}
			if builtInServicePlugin(i.Package.ID) && i.Package.ID != "web-search" && i.Package.ID != mediaServicePluginID && i.Package.ID != shareServicePluginID {
				if tools := p.dedicatedPluginToolNames(ctx, i.Package.ID); len(tools) > 0 {
					available = append(available, map[string]any{"plugin": i.Package.ID, "name": i.Package.Name, "access": "dedicated_tools", "tools": tools})
				}
				continue
			}
			if i.Package.ID == shareServicePluginID {
				if shareAvailable {
					available = append(available, map[string]any{"plugin": i.Package.ID, "name": i.Package.Name, "runtime": h.Status(i.Package.ID), "capabilities": p.shareServiceCapabilities(ctx)})
				}
				continue
			}
			accepted := []pluginhost.Capability{}
			switch i.Package.ID {
			case "web-search", mediaServicePluginID:
				caps, e := store.Capabilities(ctx, i.Package.ID)
				if e != nil {
					continue
				}
				for _, cap := range caps {
					allowed := i.Package.ID == "web-search" && p.serviceWebAllowed(cap.ID) || i.Package.ID == mediaServicePluginID && p.mediaServiceCapabilityAvailable(ctx, cap.ID)
					if cap.Discoverable && cap.Implementation == "ready" && allowed && !cap.Async {
						if i.Package.ID == mediaServicePluginID {
							cap = mediaServiceCapabilityView(cap)
						}
						accepted = append(accepted, cap)
					}
				}
			default:
				caps, e := p.dynamicPluginCapabilities(ctx, i.Package.ID)
				if e != nil {
					continue
				}
				accepted = caps
			}
			if len(accepted) > 0 {
				available = append(available, map[string]any{"plugin": i.Package.ID, "name": i.Package.Name, "runtime": h.Status(i.Package.ID), "capabilities": accepted})
			}
		}
		result = map[string]any{"plugins": available, "runtime_available": ready}
	case "service_plugin_task":
		var in struct {
			ID string `json:"task_id"`
		}
		if json.Unmarshal(raw, &in) != nil {
			return "", errors.New("invalid task arguments")
		}
		job, e := store.Job(ctx, in.ID)
		if e != nil {
			return "", errors.New("service task unavailable")
		}
		if job.Plugin == mediaServicePluginID {
			return "", errors.New("media_service_tasks_are_not_retained")
		}
		var taskPolicy pluginruntime.CallPolicy = policy
		if job.Plugin != "web-search" {
			dynamic, policyErr := p.newDynamicPluginPolicy(ctx, job.Plugin, job.Tool, job.Context)
			if policyErr != nil {
				return "", policyErr
			}
			taskPolicy = dynamic
		}
		if e = taskPolicy.Check(ctx, job, pluginhost.Capability{}); e != nil {
			return "", e
		}
		result = job
	case "service_plugin_call":
		var in struct {
			Plugin     string          `json:"plugin"`
			Capability string          `json:"capability"`
			Arguments  json.RawMessage `json:"arguments"`
			Key        string          `json:"request_key"`
		}
		if json.Unmarshal(raw, &in) != nil || !ready {
			return "", errors.New("service runtime or arguments unavailable")
		}
		if p.privateSurfOrigin != nil && (in.Plugin != "web-search" || in.Capability != "web.search" && in.Capability != "web.read" && in.Capability != "web.discover") {
			return "", errors.New("surf_read_only_capability_denied")
		}
		if in.Plugin == mediaServicePluginID {
			if in.Capability != "media.observe" && in.Capability != "media.transcribe" && in.Capability != "media.listen" {
				return "", errors.New("media_service_capability_unavailable")
			}
			if in.Key == "" {
				return "", errors.New("service request key required")
			}
			mediaResult, mediaErr := p.callMediaServicePlugin(ctx, in.Capability, in.Key, in.Arguments)
			if mediaErr != nil {
				return "", mediaErr
			}
			result = mediaResult
			break
		}
		if in.Plugin == shareServicePluginID {
			if (in.Capability != "share.resolve_public" && in.Capability != "share.resolve_channels") || in.Key == "" {
				return "", errors.New("share_service_capability_unavailable")
			}
			shareResult, shareErr := p.callShareServicePlugin(ctx, in.Capability, in.Key, in.Arguments)
			if shareErr != nil {
				return "", shareErr
			}
			result = shareResult
			break
		}
		epoch, e := p.serviceEvidenceEpoch(ctx)
		if e != nil {
			return "", e
		}
		inv := pluginhost.Invocation{Account: p.trace.Account, Thread: p.trace.Thread, Requester: p.serviceRequester(), Purpose: "conversation service request", RequestKey: in.Key, EvidenceEpoch: epoch, Authorization: p.serviceAuthorization(), SourceVersions: map[string]int{}}
		expectedEpoch, expectedAuth := epoch, p.MemoryResultAuthorization()
		if p.privateSurfOrigin != nil {
			inv.Purpose = "private autonomous public web browsing"
			inv.SourceVersions = map[string]int{"host:surf-claim:" + p.privateSurfOrigin.claim: p.privateSurfOrigin.revision + 1}
			expectedEpoch, expectedAuth = "", p.privateSurfAuthorization()
		}
		if g, ok := companion.GenerationEvidenceFromContext(ctx); ok && (g.RecallEpoch != expectedEpoch || g.Authorization != expectedAuth) {
			return "", &mcptools.WebError{Code: "generation_evidence_changed", Stage: "authorization"}
		}
		if p.trace.Source != nil && p.trace.Source.EventID != "" {
			inv.SourceVersions[p.trace.Source.EventID] = p.trace.Source.Revision
		}
		if p.trace.ReplyContext != nil {
			for id, rev := range p.trace.ReplyContext.SourceVersions {
				inv.SourceVersions[id] = rev
			}
		}
		request := pluginruntime.CallRequest{Plugin: in.Plugin, Capability: in.Capability, Context: inv, Arguments: in.Arguments}
		var invokePolicy pluginruntime.CallPolicy = policy
		if in.Plugin != "web-search" {
			dynamic, policyErr := p.newDynamicPluginPolicy(ctx, in.Plugin, in.Capability, inv)
			if policyErr != nil {
				return "", policyErr
			}
			invokePolicy = dynamic
		}
		// An already enabled plugin may be started after host restart. This never
		// activates a disabled installation or changes an administrator grant.
		if e = invokePolicy.Check(ctx, pluginhost.Job{Plugin: in.Plugin, Tool: in.Capability, Context: inv, Arguments: in.Arguments}, pluginhost.Capability{}); e != nil {
			return "", e
		}
		installed, e := store.Installed(ctx, in.Plugin)
		if e != nil || !installed.Enabled || installed.Removed {
			return "", errors.New("service plugin not enabled")
		}
		if e = h.EnsureStarted(ctx, in.Plugin, installed.Revision); e != nil {
			return "", e
		}
		out, e := h.Invoke(ctx, request, invokePolicy)
		if e != nil {
			if out.Job.ID != "" {
				// The runner discards a tool's result when it returns an error.
				// Preserve the host-issued continuation in its safe error instead.
				b, _ := json.Marshal(map[string]any{"task_id": out.Job.ID, "state": out.Job.State, "error": "service_call_not_completed", "instruction": "Read original service task; do not resubmit with a new key."})
				return string(b), &serviceTaskError{jobID: out.Job.ID, cause: e}
			}
			return "", e
		}
		if out.Job.State == "failed" {
			var failure struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(out.Job.Result, &failure)
			if failure.Error == "" {
				failure.Error = "service_failed"
			}
			b, _ := json.Marshal(out)
			return string(b), &mcptools.WebError{Code: failure.Error, Stage: "plugin"}
		}
		result = out
	default:
		return "", errors.New("unknown service tool")
	}
	b, err := json.Marshal(result)
	return string(b), err
}

type serviceTaskError struct {
	jobID string
	cause error
}

func (e *serviceTaskError) Unwrap() error { return e.cause }
func (e *serviceTaskError) Error() string { return "service_call_not_completed" }
func (e *serviceTaskError) SafeToolFailure() (string, string) {
	return e.Error(), "工具调用未完成。使用 service_plugin_task 查询原 task_id=" + e.jobID + "，保留原 request_key，先核对结果再决定下一步。"
}

type servicePluginPolicy struct {
	p        *permittedTools
	observed map[string]bool
	// deepseek is private, per-invocation evidence produced by the fixed host
	// route. A plugin result has to be a faithful page of this evidence; its
	// claimed URL/items are never enough on their own.
	deepseek map[string]deepSeekSearchEvidence
	post     pluginruntime.RouteTransport
	read     func(context.Context, string, int64, time.Duration) ([]byte, string, string, error)
}

type deepSeekSearchItem struct {
	URL     string
	Title   string
	Summary string
}

type deepSeekSearchEvidence struct {
	Query string
	Items []deepSeekSearchItem
}

type serviceWebItem struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// serviceWebReadCoverage is deliberately separate from page metadata. A
// search title, a discovery summary, or a failed fetch must never look like a
// page-body observation when the result is retained as an experience.
type serviceWebReadCoverage struct {
	Kind            string `json:"kind"`
	StartOffset     int    `json:"start_offset"`
	EndOffset       int    `json:"end_offset"`
	TotalCharacters int    `json:"total_characters"`
	Complete        bool   `json:"complete"`
	Excerpt         string `json:"excerpt"`
}

type serviceWebResult struct {
	OK                 bool                     `json:"ok"`
	Backend            string                   `json:"backend"`
	Untrusted          bool                     `json:"untrusted_external_content"`
	URL                string                   `json:"url"`
	Source             string                   `json:"source"`
	Query              string                   `json:"query"`
	Title              string                   `json:"title"`
	Text               string                   `json:"text"`
	SHA                string                   `json:"sha256"`
	Offset             int                      `json:"offset"`
	RequestedLimit     int                      `json:"requested_limit"`
	ReturnedCharacters int                      `json:"returned_characters"`
	TotalCharacters    int                      `json:"total_characters"`
	NextOffset         *int                     `json:"next_offset"`
	Complete           bool                     `json:"complete"`
	ReadCoverage       *serviceWebReadCoverage  `json:"read_coverage"`
	Items              []serviceWebItem         `json:"items"`
	ProviderItems      int                      `json:"provider_items"`
	RequestedCount     int                      `json:"requested_count"`
	EffectiveCount     int                      `json:"effective_provider_count"`
	ProviderExhausted  bool                     `json:"provider_page_exhausted"`
	Fallback           bool                     `json:"fallback_used"`
	Candidates         []mcptools.DiscoveryItem `json:"candidates"`
	MetadataOnly       bool                     `json:"metadata_only"`
	TotalItems         int                      `json:"total_items"`
}

func (s servicePluginPolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	p := s.p
	if p.privateSurfOrigin != nil {
		if err := p.privateSurfOrigin.check(ctx, p.trace); err != nil {
			return err
		}
	}
	if p.manager.draining() {
		return errors.New("web_host_draining")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if j.Plugin != "web-search" || (j.Tool != "web.search" && j.Tool != "web.read" && j.Tool != "web.discover") || cap.Async {
		return errors.New("service capability adapter unavailable")
	}
	if !p.serviceWebAllowed(j.Tool) {
		return errors.New("public web capability disabled")
	}
	if j.Tool == "web.discover" {
		if _, _, err := p.webDiscoveryRequest(j.Arguments); err != nil {
			return err
		}
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return err
	}
	installed, err := store.Installed(ctx, j.Plugin)
	if err != nil || !installed.Enabled || installed.Removed {
		return errors.New("service plugin authorization revoked")
	}
	if j.PackageDigest != "" && (j.PackageDigest != installed.Digest || j.Version != installed.Package.Version) {
		return errors.New("service plugin package changed")
	}
	configuration, err := store.ConfigurationForExecution(ctx, j.Plugin)
	if err != nil || j.ConfigRevision != "" && j.ConfigRevision != strconv.Itoa(configuration.Revision) {
		return errors.New("service plugin configuration changed")
	}
	if pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: store.Account(), Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, j.Context, []string{"network"}) != nil {
		return errors.New("service plugin permission revoked")
	}
	if j.ID != "" && j.State != "" {
		current, err := store.Job(ctx, j.ID)
		if err != nil || current.Revision != j.Revision || current.CancelRequested || current.State == "cancelled" {
			return errors.New("service plugin task changed or cancelled")
		}
	}
	if !p.servicePluginsAllowed() || j.Context.Account != p.trace.Account || j.Context.Thread != p.trace.Thread || j.Context.Requester != p.serviceRequester() || !p.manager.agentTaskActive(p.trace) {
		return errors.New("service request authorization changed")
	}
	if j.Context.Authorization == "" || j.Context.Authorization != p.serviceAuthorization() {
		return errors.New("service evidence authorization changed")
	}
	epoch, e := p.serviceEvidenceEpoch(ctx)
	if e != nil {
		return e
	}
	if j.Context.EvidenceEpoch == "" || j.Context.EvidenceEpoch != epoch {
		return errors.New("service evidence epoch changed")
	}
	if p.privateSurfOrigin != nil {
		if j.Context.Purpose != "private autonomous public web browsing" || len(j.Context.SourceVersions) != 1 || j.Context.SourceVersions["host:surf-claim:"+p.privateSurfOrigin.claim] != p.privateSurfOrigin.revision+1 {
			return errors.New("surf_claim_proof_changed")
		}
		return nil
	}
	for id, rev := range j.Context.SourceVersions {
		ev, e := p.manager.memory.ReadEvent(ctx, []string{companion.ScopeID("wechat", j.Context.Account, j.Context.Thread)}, id)
		if e != nil || ev.Revision != rev {
			return errors.New("service source unavailable or changed")
		}
	}
	return nil
}

func (p *permittedTools) serviceAuthorization() string {
	if p.privateSurfOrigin != nil {
		return p.privateSurfAuthorization()
	}
	if !p.servicePluginsAllowed() {
		return ""
	}
	a := p.manager.agent.config()
	b, _ := json.Marshal([]any{p.trace.Account, p.trace.Channel, p.trace.Thread, p.serviceRequester(), p.trace.Surf, p.memoryQueryAuthorization(), a.Enabled, a.ExternalOwnerOnly, p.serviceWebAllowed("web.search"), p.serviceWebAllowed("web.read")})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (p *permittedTools) serviceEvidenceEpoch(ctx context.Context) (string, error) {
	if p.privateSurfOrigin != nil {
		if err := p.privateSurfOrigin.check(ctx, p.trace); err != nil {
			return "", err
		}
		return "surf:" + p.privateSurfOrigin.claim, nil
	}
	return p.manager.memory.RecallEpoch(ctx)
}

func (p *permittedTools) serviceWebAllowed(tool string) bool {
	p.manager.mu.Lock()
	defer p.manager.mu.Unlock()
	c := p.manager.mcp
	return c.Enabled && (tool == "web.search" && c.WebSearchEnabled || (tool == "web.read" || tool == "web.discover" && p.trace.Surf) && c.WebFetchEnabled)
}

func (p *permittedTools) validateServiceOutput(ctx context.Context, name, result string) error {
	if name != "service_plugin_call" && name != "service_plugin_task" {
		if webServiceCapability(name) == "" {
			return nil
		}
		if installed, _ := p.installedWebState(); !installed {
			return nil
		}
	}
	var envelope struct {
		Job    pluginhost.Job `json:"job"`
		ID     string         `json:"id"`
		TaskID string         `json:"task_id"`
	}
	if json.Unmarshal([]byte(result), &envelope) != nil {
		return errors.New("invalid service output")
	}
	id := envelope.Job.ID
	if id == "" {
		id = envelope.ID
	}
	if id == "" {
		id = envelope.TaskID
	}
	if id == "" {
		return errors.New("service output has no original task")
	}
	store, e := p.manager.packageStore()
	if e != nil {
		return e
	}
	j, e := store.Job(ctx, id)
	if e != nil {
		return e
	}
	if j.Plugin == "web-search" {
		return (servicePluginPolicy{p: p}).Check(ctx, j, pluginhost.Capability{})
	}
	dynamic, err := p.newDynamicPluginPolicy(ctx, j.Plugin, j.Tool, j.Context)
	if err != nil {
		return err
	}
	return dynamic.Check(ctx, j, pluginhost.Capability{})
}

// deepSeekConfiguration is intentionally separate from the generic public-web
// route. Selecting a provider is an administrator configuration decision: a
// configured chat/model credential is never repurposed as a search credential.
func (s servicePluginPolicy) deepSeekConfiguration(ctx context.Context, j pluginhost.Job) error {
	store, err := s.p.manager.packageStore()
	if err != nil {
		return err
	}
	config, err := store.ConfigurationForExecution(ctx, j.Plugin)
	if err != nil || strconv.Itoa(config.Revision) != j.ConfigRevision {
		return errors.New("deepseek search configuration changed")
	}
	var backend string
	if json.Unmarshal(config.Values["search_backend"], &backend) != nil || backend != "deepseek" {
		return errors.New("deepseek search backend not selected")
	}
	var key string
	if json.Unmarshal(config.Values["search_key"], &key) != nil || strings.TrimSpace(key) == "" {
		return errors.New("deepseek search credential unavailable")
	}
	return nil
}

func strictServicePluginJSON(raw json.RawMessage, target any) error {
	return strictjson.DecodeKnownFields(raw, target)
}

func publicSearchURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", false
	}
	return raw, true
}

func cleanSearchText(text string) string { return strings.Join(strings.Fields(text), " ") }

func deepSeekEvidenceFromReply(query string, raw json.RawMessage) (deepSeekSearchEvidence, error) {
	var reply struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Summary *string `json:"summary"`
			Snippet *string `json:"snippet"`
		} `json:"results"`
	}
	if err := strictServicePluginJSON(raw, &reply); err != nil {
		return deepSeekSearchEvidence{}, errors.New("deepseek search response invalid")
	}
	items := make([]deepSeekSearchItem, 0, len(reply.Results))
	seen := map[string]bool{}
	for _, result := range reply.Results {
		link, ok := publicSearchURL(result.URL)
		if !ok || seen[link] {
			continue
		}
		// This is deliberately the same normalization as the official runtime.
		// Provider data remains untrusted and is never treated as a host fact.
		summary := ""
		if result.Summary != nil {
			summary = *result.Summary
		} else if result.Snippet != nil {
			summary = *result.Snippet
		}
		seen[link] = true
		items = append(items, deepSeekSearchItem{URL: link, Title: cleanSearchText(result.Title), Summary: cleanSearchText(summary)})
	}
	return deepSeekSearchEvidence{Query: query, Items: items}, nil
}

func (s servicePluginPolicy) routeTransport() pluginruntime.RouteTransport {
	if s.post != nil {
		return s.post
	}
	return mcptools.PostPluginRoute
}

func (s servicePluginPolicy) deepSeekRoute(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (json.RawMessage, error) {
	if s.deepseek == nil {
		return nil, errors.New("deepseek evidence sink unavailable")
	}
	if err := s.deepSeekConfiguration(ctx, j); err != nil {
		return nil, err
	}
	store, err := s.p.manager.packageStore()
	if err != nil {
		return nil, err
	}
	var args struct {
		Query   string `json:"query"`
		Count   *int   `json:"count"`
		Offset  *int   `json:"offset"`
		Limit   *int   `json:"limit"`
		Max     *int64 `json:"max_bytes"`
		Timeout *int64 `json:"timeout_seconds"`
	}
	if strictServicePluginJSON(j.Arguments, &args) != nil || len(strings.TrimSpace(args.Query)) == 0 || args.Count != nil && *args.Count < 1 || args.Max != nil && *args.Max < 1 || args.Timeout != nil && *args.Timeout < 1 {
		return nil, errors.New("deepseek request arguments invalid")
	}
	var envelope struct {
		Route    string          `json:"route"`
		Body     json.RawMessage `json:"body"`
		MaxBytes int64           `json:"max_bytes"`
		Timeout  int64           `json:"timeout_seconds"`
	}
	if strictServicePluginJSON(raw, &envelope) != nil {
		return nil, errors.New("deepseek route arguments invalid")
	}
	expectedMax, expectedTimeout := int64(128<<10), int64(18)
	if args.Max != nil {
		expectedMax = *args.Max
	}
	if args.Timeout != nil {
		expectedTimeout = *args.Timeout
	}
	if envelope.MaxBytes != expectedMax || envelope.Timeout != expectedTimeout {
		return nil, errors.New("deepseek route limits changed")
	}
	return pluginruntime.ProxyHTTPRoute(ctx, store, j, cap, raw, func(route string, body json.RawMessage) error {
		if route != "deepseek-search" {
			return errors.New("deepseek route unavailable")
		}
		var fields map[string]json.RawMessage
		if strictServicePluginJSON(body, &fields) != nil || len(fields) != 3 || fields["query"] == nil || fields["count"] == nil || fields["need_content"] == nil {
			return errors.New("deepseek route body invalid")
		}
		var bound struct {
			Query       string `json:"query"`
			Count       int    `json:"count"`
			NeedContent bool   `json:"need_content"`
		}
		if strictServicePluginJSON(body, &bound) != nil || bound.Query != strings.TrimSpace(args.Query) || len([]rune(bound.Query)) == 0 || bound.Count < 1 || bound.NeedContent || args.Count != nil && bound.Count != *args.Count {
			return errors.New("deepseek route source binding changed")
		}
		return nil
	}, func(ctx context.Context, endpoint string, headers map[string]string, body json.RawMessage, maxBytes int64, timeout time.Duration) (json.RawMessage, error) {
		reply, err := s.routeTransport()(ctx, endpoint, headers, body, maxBytes, timeout)
		if err != nil {
			return nil, err
		}
		// The runtime returns the original query in its result while its provider
		// body uses the trimmed form. Retain the former for result binding.
		evidence, err := deepSeekEvidenceFromReply(args.Query, reply)
		if err != nil {
			return nil, err
		}
		s.deepseek[j.ID] = evidence
		return reply, nil
	})
}
func (s servicePluginPolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	// Only text web observations are connected in this migration stage. Media,
	// async jobs and arbitrary plugin result envelopes need dedicated validators.
	if j.Plugin != "web-search" || (j.Tool != "web.search" && j.Tool != "web.read" && j.Tool != "web.discover") || cap.Async {
		return pluginruntime.VerifiedResult{}, errors.New("service result adapter unavailable")
	}
	if err := s.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var failure struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	failed := json.NewDecoder(bytes.NewReader(raw))
	failed.DisallowUnknownFields()
	if failed.Decode(&failure) == nil && failure.OK != nil && !*failure.OK {
		// Keep bounded machine codes only, never arbitrary exception prose.
		if len(failure.Error) == 0 || len(failure.Error) > 96 {
			return pluginruntime.VerifiedResult{}, errors.New("invalid plugin failure")
		}
		for _, r := range failure.Error {
			if r != '_' && (r < 'a' || r > 'z') {
				return pluginruntime.VerifiedResult{}, errors.New("invalid plugin failure code")
			}
		}
		return pluginruntime.VerifiedResult{Failed: true, Result: raw}, nil
	}
	var result serviceWebResult
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || !result.OK || !result.Untrusted {
		return pluginruntime.VerifiedResult{}, errors.New("public content reading failed")
	}
	if j.Tool == "web.read" {
		if err := validateWebReadResult(j.Arguments, result, s.observed); err != nil {
			// Jobs produced by the installed pre-0.3.4 runtime remain readable:
			// their old result schema has no coverage proof. Keep their previous
			// verified delivery semantics, but do not let the missing proof become
			// a page-body experience in the new surf ledger.
			if webReadCoverageRequired(j.Version) || validateLegacyWebReadResult(j.Arguments, result, s.observed) != nil {
				return pluginruntime.VerifiedResult{}, err
			}
		}
	}
	if j.Tool == "web.discover" {
		if err := s.p.validateWebDiscoveryResult(j.Arguments, result, s.observed); err != nil {
			return pluginruntime.VerifiedResult{}, err
		}
	}
	if j.Tool == "web.search" {
		var args struct {
			Query   string `json:"query"`
			Source  string `json:"source"`
			Count   *int   `json:"count"`
			Offset  *int   `json:"offset"`
			Limit   *int   `json:"limit"`
			Max     *int64 `json:"max_bytes"`
			Timeout *int64 `json:"timeout_seconds"`
		}
		if strictServicePluginJSON(j.Arguments, &args) != nil || args.Query != result.Query {
			return pluginruntime.VerifiedResult{}, errors.New("plugin query binding changed")
		}
		if args.Source == "bilibili" {
			if result.Backend != "bilibili" || !s.observed[result.Source] {
				return pluginruntime.VerifiedResult{}, errors.New("bilibili result source not observed")
			}
			for _, item := range result.Items {
				if !strings.HasPrefix(item.URL, "https://www.bilibili.com/video/BV") || !mcptools.IsPublicVideoPage(item.URL) {
					return pluginruntime.VerifiedResult{}, errors.New("unexpected bilibili result URL")
				}
			}
		} else if result.Backend == "deepseek" {
			if err := s.validateDeepSeekResult(ctx, j, args.Query, args.Count, args.Offset, args.Limit, result); err != nil {
				return pluginruntime.VerifiedResult{}, err
			}
		} else if result.Backend != "public" || !s.observed[result.Source] {
			return pluginruntime.VerifiedResult{}, errors.New("plugin result source not observed")
		}
	}
	return pluginruntime.VerifiedResult{Result: append(json.RawMessage(nil), raw...)}, nil
}

func webReadCoverageRequired(version string) bool {
	base := strings.SplitN(version, "-", 2)[0]
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return true
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	patch, patchErr := strconv.Atoi(parts[2])
	if majorErr != nil || minorErr != nil || patchErr != nil || major < 0 || minor < 0 || patch < 0 {
		return true
	}
	return major > 0 || major == 0 && (minor > 3 || minor == 3 && patch >= 4)
}

type webReadArguments struct {
	URL         string  `json:"url"`
	Offset      *int    `json:"offset"`
	Limit       *int    `json:"limit"`
	ExpectedSHA *string `json:"expected_sha256"`
	MaxBytes    *int64  `json:"max_bytes"`
	Timeout     *int64  `json:"timeout_seconds"`
}

func parseWebReadArguments(raw json.RawMessage) (webReadArguments, error) {
	var in webReadArguments
	if strictServicePluginJSON(raw, &in) != nil || in.URL == "" || in.Offset != nil && *in.Offset < 0 || in.Limit != nil && *in.Limit < 1 || in.MaxBytes != nil && *in.MaxBytes < 1 || in.Timeout != nil && *in.Timeout < 1 {
		return in, errors.New("web_read_arguments_invalid")
	}
	return in, nil
}

func validateLegacyWebReadResult(raw json.RawMessage, result serviceWebResult, observed map[string]bool) error {
	in, err := parseWebReadArguments(raw)
	if err != nil || result.ReadCoverage != nil || !observed[result.URL] || result.MetadataOnly || in.ExpectedSHA != nil && *in.ExpectedSHA != "" && result.SHA != *in.ExpectedSHA {
		return errors.New("legacy_web_read_unverified")
	}
	return nil
}

func boundedWebReadExcerpt(text string) string {
	runes := []rune(text)
	if len(runes) > 320 {
		runes = runes[:320]
	}
	return string(runes)
}

func validateWebReadResult(raw json.RawMessage, result serviceWebResult, observed map[string]bool) error {
	in, err := parseWebReadArguments(raw)
	if err != nil {
		return err
	}
	offset, limit := 0, 10000
	if in.Offset != nil {
		offset = *in.Offset
	}
	if in.Limit != nil {
		limit = *in.Limit
	}
	coverage := result.ReadCoverage
	textCharacters := utf8.RuneCountInString(result.Text)
	if result.TotalCharacters < offset {
		return errors.New("web_read_coverage_unverified")
	}
	expectedCharacters := min(limit, result.TotalCharacters-offset)
	if !observed[result.URL] || result.MetadataOnly || coverage == nil || coverage.Kind != "page_body" ||
		result.Offset != offset || result.RequestedLimit != limit || result.ReturnedCharacters != textCharacters || textCharacters != expectedCharacters ||
		coverage.StartOffset != offset || coverage.EndOffset != offset+textCharacters || coverage.TotalCharacters != result.TotalCharacters ||
		coverage.Complete != result.Complete || coverage.Excerpt != boundedWebReadExcerpt(result.Text) ||
		len(coverage.Excerpt) > 4*320 || !utf8.ValidString(coverage.Excerpt) || len(result.SHA) != 64 || in.ExpectedSHA != nil && *in.ExpectedSHA != "" && result.SHA != *in.ExpectedSHA {
		return errors.New("web_read_coverage_unverified")
	}
	for _, r := range result.SHA {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return errors.New("web_read_coverage_unverified")
		}
	}
	end := offset + textCharacters
	if result.Complete != (end == result.TotalCharacters) || end < result.TotalCharacters && (result.NextOffset == nil || *result.NextOffset != end) || end == result.TotalCharacters && result.NextOffset != nil {
		return errors.New("web_read_continuation_changed")
	}
	return nil
}

func (s servicePluginPolicy) validateDeepSeekResult(ctx context.Context, j pluginhost.Job, query string, count, offset, limit *int, result serviceWebResult) error {
	if err := s.deepSeekConfiguration(ctx, j); err != nil {
		return err
	}
	evidence, ok := s.deepseek[j.ID]
	if !ok || evidence.Query != query || result.Fallback || result.RequestedCount < 1 || result.EffectiveCount != result.RequestedCount {
		return errors.New("deepseek result is not backed by host response")
	}
	if count != nil && *count != result.RequestedCount {
		return errors.New("deepseek count binding changed")
	}
	start, page := 0, 8
	if offset != nil {
		start = *offset
	}
	if limit != nil {
		page = *limit
	}
	if start < 0 || page < 1 || start > len(evidence.Items) || result.RequestedLimit != page || result.ProviderItems != len(evidence.Items) {
		return errors.New("deepseek result page binding changed")
	}
	end := start + page
	if end > len(evidence.Items) {
		end = len(evidence.Items)
	}
	if len(result.Items) != end-start || result.ProviderExhausted != (end == len(evidence.Items)) {
		return errors.New("deepseek result page changed")
	}
	if end < len(evidence.Items) {
		if result.NextOffset == nil || *result.NextOffset != end {
			return errors.New("deepseek continuation changed")
		}
	} else if result.NextOffset != nil {
		return errors.New("deepseek unexpected continuation")
	}
	for n, item := range result.Items {
		want := evidence.Items[start+n]
		if item.URL != want.URL || item.Title != want.Title || item.Summary != want.Summary {
			return errors.New("deepseek result item not observed")
		}
	}
	return nil
}
func (s servicePluginPolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	if err := s.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	if method == "wechat/http_route" {
		if j.Tool != "web.search" {
			return nil, errors.New("host service unavailable")
		}
		return s.deepSeekRoute(ctx, j, cap, raw)
	}
	if method != "wechat/http_read" || (j.Tool != "web.read" && j.Tool != "web.search" && j.Tool != "web.discover") {
		return nil, errors.New("host service unavailable")
	}
	var in struct {
		URL      string `json:"url"`
		MaxBytes int64  `json:"max_bytes"`
		Timeout  int64  `json:"timeout_seconds"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || in.Timeout <= 0 || in.Timeout > int64((1<<63-1)/time.Second) {
		return nil, errors.New("invalid public read arguments")
	}
	if err := s.validatePublicWebRead(ctx, j, in.URL, in.MaxBytes, in.Timeout); err != nil {
		return nil, err
	}
	if s.observed == nil {
		return nil, errors.New("web_read_evidence_sink_unavailable")
	}
	read := s.read
	if read == nil {
		read = mcptools.ReadPublicPluginPage
	}
	body, kind, url, err := read(ctx, in.URL, in.MaxBytes, time.Duration(in.Timeout)*time.Second)
	if err != nil {
		code := "public_read_failed"
		var web *mcptools.WebError
		if errors.As(err, &web) {
			code = web.Code
		}
		return json.Marshal(map[string]any{"ok": false, "error": code})
	}
	if err := s.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	s.observed[url] = true
	return json.Marshal(map[string]any{"ok": true, "url": url, "content_type": kind, "body_base64": base64.StdEncoding.EncodeToString(body)})
}
