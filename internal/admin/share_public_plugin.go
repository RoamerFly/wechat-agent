package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/strictjson"
)

type publicShareObservation struct {
	OK                      bool     `json:"ok"`
	Platform                string   `json:"platform"`
	SourceURL               string   `json:"source_url"`
	Title                   string   `json:"title"`
	ObjectID                string   `json:"object_id"`
	DurationMS              *int64   `json:"duration_ms"`
	Text                    string   `json:"text"`
	SHA                     string   `json:"content_sha256"`
	Offset                  int      `json:"offset"`
	RequestedLimit          int      `json:"requested_limit"`
	TotalCharacters         int      `json:"total_characters"`
	NextOffset              *int     `json:"next_offset"`
	CandidateOffset         int      `json:"candidate_offset"`
	RequestedCandidateLimit int      `json:"requested_candidate_limit"`
	TotalCandidates         int      `json:"total_candidates"`
	NextCandidateOffset     *int     `json:"next_candidate_offset"`
	Locators                []string `json:"private_media_locators"`
	Coverage                string   `json:"coverage"`
	Observed                *bool    `json:"video_observed"`
	Downloaded              *bool    `json:"media_downloaded"`
	Untrusted               bool     `json:"untrusted_external_content"`
	Complete                bool     `json:"complete"`
}

type publicShareRead func(context.Context, string, int64, time.Duration) ([]byte, string, string, error)

type publicSharePolicy struct {
	*receivedSharePolicy
	requestedURL  string
	videoRequired bool
	mu            sync.Mutex
	read          publicShareRead
	media         func(context.Context, string, string, int64) ([]byte, string, error)
	reads         int
	bytes         int64
	pages         map[string]string
	locators      map[string]bool
}

func decodeShareJSON(raw []byte) (any, error) { return strictjson.Decode(raw) }

func sameShareBody(raw json.RawMessage, expected any) bool { return strictjson.Matches(raw, expected) }

func (m *Manager) newPublicSharePolicy(ctx context.Context, message linuxnative.Message) (*publicSharePolicy, error) {
	base, err := m.newReceivedSharePolicyFor(ctx, message, "share.resolve_public")
	if err != nil {
		return nil, err
	}
	return &publicSharePolicy{receivedSharePolicy: base, requestedURL: message.Link.URL, videoRequired: *message.AppMessageType == 4, read: mcptools.ReadPublicPluginShareSource, media: mcptools.GetPublicPluginMedia, pages: map[string]string{}, locators: map[string]bool{}}, nil
}

// newExplicitPublicSharePolicy keeps an actual incoming source as evidence,
// while binding an explicit public URL as the requested purpose. In particular
// it never turns a text message into a fabricated native share card.
func (m *Manager) newExplicitPublicSharePolicy(ctx context.Context, source linuxnative.Message, rawURL string, videoRequired bool) (*publicSharePolicy, error) {
	requestedURL := sharePublicURL(rawURL)
	if requestedURL == "" {
		return nil, errors.New("share_public_url_invalid")
	}
	base, err := m.newExplicitPublicURLSourcePolicy(ctx, source)
	if err != nil {
		return nil, err
	}
	base.requestSuffix = agentID("explicit-public-url", requestedURL, strconv.FormatBool(videoRequired))
	return &publicSharePolicy{receivedSharePolicy: base, requestedURL: requestedURL, videoRequired: videoRequired, read: mcptools.ReadPublicPluginShareSource, media: mcptools.GetPublicPluginMedia, pages: map[string]string{}, locators: map[string]bool{}}, nil
}

func (p *publicSharePolicy) Check(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability) error {
	if err := p.receivedSharePolicy.Check(ctx, j, cap); err != nil {
		return err
	}
	p.manager.mu.Lock()
	allowed := p.manager.mcp.Enabled && p.manager.mcp.WebFetchEnabled
	p.manager.mu.Unlock()
	if !allowed {
		return errors.New("share_public_read_disabled")
	}
	// Revalidate the installed grant/configuration even after the private result
	// has been recovered: they may change while a media download is in flight.
	if j.PackageDigest != "" {
		i, err := p.store.Installed(ctx, j.Plugin)
		if err != nil || i.Digest != j.PackageDigest || i.Package.Version != j.Version {
			return errors.New("share_plugin_version_changed")
		}
		config, err := p.store.Configuration(ctx, j.Plugin)
		if err != nil || strconv.Itoa(config.Revision) != j.ConfigRevision {
			return errors.New("share_plugin_configuration_changed")
		}
		if err := pluginhost.Authorize(i.Package, pluginhost.Grant{Account: p.message.AccountID, Plugin: i.Package.ID, Version: i.Package.Version, Permissions: i.Permissions}, j.Context, i.Package.Permissions); err != nil {
			return errors.New("share_plugin_permissions_changed")
		}
	}
	if j.ID != "" {
		current, err := p.store.Job(ctx, j.ID)
		if err != nil || current.CancelRequested || current.Revision != j.Revision {
			return errors.New("share_plugin_task_changed")
		}
	}
	return nil
}

func originalPublicShareURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

var publicShareJSONString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
var publicShareLiteralURL = regexp.MustCompile(`https?://[^\s<>"'\\]+`)
var publicShareHTMLAttribute = regexp.MustCompile(`(?is)(?:src|href|content)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
var publicShareErrorCode = regexp.MustCompile(`^[a-z_]{1,96}$`)

// Record literal locations from actual reads, including JSON-escaped URLs and
// HTML attributes. The approved parser verifies platform/work identity; this
// host check independently rejects invented or cross-call media locations.
func publicShareLocations(body []byte, base string) map[string]bool {
	out := map[string]bool{}
	add := func(raw string) {
		if len(raw) > 16384 {
			return
		}
		if value := originalPublicShareURL(raw); value != "" {
			out[value] = true
		}
	}
	text := string(body)
	for _, raw := range publicShareLiteralURL.FindAllString(text, -1) {
		add(html.UnescapeString(raw))
	}
	for _, token := range publicShareJSONString.FindAllString(text, -1) {
		var decoded string
		if json.Unmarshal([]byte(token), &decoded) == nil {
			add(decoded)
		}
	}
	u, err := url.Parse(base)
	if err == nil {
		for _, match := range publicShareHTMLAttribute.FindAllStringSubmatch(text, -1) {
			value := match[1]
			if value == "" {
				value = match[2]
			}
			if v, e := url.Parse(html.UnescapeString(value)); e == nil {
				add(u.ResolveReference(v).String())
			}
		}
	}
	return out
}

func (p *publicSharePolicy) Service(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, method string, raw json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	if method != "wechat/http_read" {
		return nil, errors.New("share_host_service_unavailable")
	}
	var in struct {
		URL      string `json:"url"`
		MaxBytes int64  `json:"max_bytes"`
		Timeout  int64  `json:"timeout_seconds"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if _, err := decodeShareJSON(raw); err != nil {
		return nil, err
	}
	if d.Decode(&in) != nil || in.MaxBytes <= 0 || in.MaxBytes > 4<<20 || in.Timeout <= 0 || in.Timeout > int64((1<<63-1)/time.Second) {
		return nil, errors.New("share_invalid_public_read")
	}
	requested := originalPublicShareURL(in.URL)
	if p.reads == 0 && requested != originalPublicShareURL(p.requestedURL) {
		return nil, errors.New("share_initial_source_mismatch")
	}
	_, knownPage := p.pages[sharePublicURL(requested)]
	if p.reads > 0 && !p.locators[requested] && !knownPage {
		return nil, errors.New("share_followup_source_unverified")
	}
	// This bounds host-side proof accumulation, not video length/download size.
	if p.bytes+in.MaxBytes > 32<<20 {
		return json.Marshal(map[string]any{"ok": false, "error": "share_read_evidence_budget_exhausted"})
	}
	body, kind, final, err := p.read(ctx, in.URL, in.MaxBytes, time.Duration(in.Timeout)*time.Second)
	if err != nil {
		return json.Marshal(map[string]any{"ok": false, "error": "public_read_failed"})
	}
	if err = p.Check(ctx, j, cap); err != nil {
		return nil, err
	}
	if int64(len(body)) > in.MaxBytes || originalPublicShareURL(final) == "" {
		return nil, errors.New("share_public_response_invalid")
	}
	p.bytes += int64(len(body))
	p.reads++
	p.pages[sharePublicURL(final)] = kind
	for locator := range publicShareLocations(body, final) {
		p.locators[locator] = true
	}
	if strings.HasPrefix(kind, "video/") || strings.HasPrefix(kind, "image/") {
		p.locators[originalPublicShareURL(final)] = true
	}
	return json.Marshal(map[string]any{"ok": true, "url": final, "content_type": kind, "body_base64": base64.StdEncoding.EncodeToString(body)})
}

func validSharePage(offset, limit, total, count int, next *int) bool {
	if offset < 0 || limit <= 0 || total < offset || count < 0 || count > limit || count > total-offset {
		return false
	}
	end := offset + count
	if end == total {
		return next == nil
	}
	return count > 0 && next != nil && *next == end
}

func (p *publicSharePolicy) Validate(ctx context.Context, j pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Check(ctx, j, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	if _, err := decodeShareJSON(raw); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var failure struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&failure) == nil && failure.OK != nil && !*failure.OK && publicShareErrorCode.MatchString(failure.Error) {
		return pluginruntime.VerifiedResult{Failed: true, Result: raw}, nil
	}
	var result publicShareObservation
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil || !result.OK || !result.Untrusted || result.Observed == nil || *result.Observed || result.Downloaded == nil || *result.Downloaded || len(raw) > 1<<20 {
		return pluginruntime.VerifiedResult{}, errors.New("share_public_result_invalid")
	}
	hash, err := hex.DecodeString(result.SHA)
	kind := strings.ToLower(p.pages[result.SourceURL])
	pageKind := strings.HasPrefix(kind, "text/") || strings.Contains(kind, "html") || strings.HasPrefix(kind, "video/") || strings.HasPrefix(kind, "image/")
	if err != nil || len(hash) != 32 || !pageKind || sharePublicURL(result.SourceURL) != result.SourceURL || result.DurationMS != nil && *result.DurationMS <= 0 {
		return pluginruntime.VerifiedResult{}, errors.New("share_public_source_unverified")
	}
	if !validSharePage(result.Offset, result.RequestedLimit, result.TotalCharacters, utf8.RuneCountInString(result.Text), result.NextOffset) || !validSharePage(result.CandidateOffset, result.RequestedCandidateLimit, result.TotalCandidates, len(result.Locators), result.NextCandidateOffset) || result.Complete != (result.NextOffset == nil && result.NextCandidateOffset == nil) {
		return pluginruntime.VerifiedResult{}, errors.New("share_public_pagination_invalid")
	}
	args := struct {
		URL             string `json:"url"`
		VideoRequired   bool   `json:"video_required"`
		Offset          int    `json:"offset"`
		Limit           int    `json:"limit"`
		CandidateOffset int    `json:"candidate_offset"`
		CandidateLimit  int    `json:"candidate_limit"`
	}{Limit: 14000, CandidateLimit: 20}
	if json.Unmarshal(j.Arguments, &args) != nil || args.URL != p.requestedURL || args.VideoRequired != p.videoRequired || result.Offset != args.Offset || result.CandidateOffset != args.CandidateOffset || result.RequestedLimit != args.Limit || result.RequestedCandidateLimit != args.CandidateLimit {
		return pluginruntime.VerifiedResult{}, errors.New("share_public_arguments_changed")
	}
	if result.Coverage == "page_text" {
		if result.Text == "" || len(result.Locators) != 0 || p.videoRequired {
			return pluginruntime.VerifiedResult{}, errors.New("share_public_video_not_observed")
		}
	} else if result.Coverage != "media_locator_only" || result.Text != "" || len(result.Locators) == 0 {
		return pluginruntime.VerifiedResult{}, errors.New("share_public_coverage_invalid")
	}
	for _, locator := range result.Locators {
		if !p.locators[locator] {
			return pluginruntime.VerifiedResult{}, errors.New("share_public_media_unverified")
		}
	}
	// Keep media URLs and body text outside the public job/log projection.
	public, _ := json.Marshal(map[string]any{"ok": true, "source_url": result.SourceURL, "coverage": result.Coverage, "video_observed": false, "media_downloaded": false, "complete": result.Complete})
	return pluginruntime.VerifiedResult{Result: public, Private: append(json.RawMessage(nil), raw...)}, nil
}

func (p *publicSharePolicy) resolve(ctx context.Context) (publicShareObservation, pluginhost.Job, error) {
	var result publicShareObservation
	j := pluginhost.Job{Plugin: "share-reader", Tool: "share.resolve_public", Context: p.invocation()}
	if err := p.Check(ctx, j, pluginhost.Capability{}); err != nil {
		return result, j, err
	}
	h, ready, err := p.manager.installedPluginRuntime()
	if err != nil || !ready {
		return result, j, errors.New("share_plugin_runtime_unavailable")
	}
	i, err := p.store.Installed(ctx, j.Plugin)
	if err != nil {
		return result, j, err
	}
	if err = h.EnsureStarted(ctx, j.Plugin, i.Revision); err != nil {
		return result, j, err
	}
	args, _ := json.Marshal(map[string]any{"url": p.requestedURL, "video_required": p.videoRequired})
	out, err := h.Invoke(ctx, pluginruntime.CallRequest{Plugin: j.Plugin, Capability: j.Tool, Context: j.Context, Arguments: args}, p)
	if err != nil {
		return result, out.Job, errors.New("share_plugin_resolution_incomplete")
	}
	if out.Job.State != "completed" {
		if out.Job.State == "failed" {
			var failure struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(out.Job.Result, &failure) == nil && publicShareErrorCode.MatchString(failure.Error) {
				return result, out.Job, errors.New(failure.Error)
			}
			return result, out.Job, errors.New("share_plugin_resolution_failed")
		}
		return result, out.Job, errors.New("share_plugin_resolution_needs_review")
	}
	private, err := p.store.PrivateJobResult(ctx, out.Job, func(ctx context.Context, j pluginhost.Job) error { return p.Check(ctx, j, pluginhost.Capability{}) })
	if err != nil || json.Unmarshal(private, &result) != nil {
		return result, out.Job, errors.New("share_plugin_handoff_unavailable")
	}
	return result, out.Job, nil
}

func (p *publicSharePolicy) readShare(ctx context.Context, limit int64) (mcptools.PublicShare, error) {
	result, job, err := p.resolve(ctx)
	if err != nil {
		return mcptools.PublicShare{}, err
	}
	return p.handoff(ctx, result, job, limit)
}

func (p *publicSharePolicy) handoff(ctx context.Context, result publicShareObservation, job pluginhost.Job, limit int64) (mcptools.PublicShare, error) {
	if limit <= 0 || limit > mcptools.MaxPublicMediaBytes {
		return mcptools.PublicShare{}, errors.New("share_media_limit_invalid")
	}
	if err := p.Check(ctx, job, pluginhost.Capability{}); err != nil {
		return mcptools.PublicShare{}, err
	}
	out := mcptools.PublicShare{PageURL: result.SourceURL, Title: result.Title}
	if result.Coverage == "page_text" {
		out.Kind, out.Text = "page", result.Text
		if result.NextOffset != nil {
			out.Text += fmt.Sprintf("\n[本次仅取得网页正文的一部分；续读位置 offset=%d，总字符数=%d，不能据此声称已读完全文。]", *result.NextOffset, result.TotalCharacters)
		}
		return out, nil
	}
	var last error
	for _, locator := range result.Locators {
		if err := p.Check(ctx, job, pluginhost.Capability{}); err != nil {
			return mcptools.PublicShare{}, err
		}
		data, kind, err := p.media(ctx, locator, result.SourceURL, limit)
		if current := p.Check(ctx, job, pluginhost.Capability{}); current != nil {
			return mcptools.PublicShare{}, current
		}
		if err != nil {
			last = err
			continue
		}
		if int64(len(data)) > limit {
			last = errors.New("share_media_limit_exceeded")
			continue
		}
		out.Kind = "video"
		if strings.HasPrefix(strings.ToLower(kind), "image/") && !p.videoRequired {
			if !p.manager.Config().Behavior.ReadImages {
				return mcptools.PublicShare{}, errors.New("media_understanding_disabled")
			}
			out.Kind = "image"
		}
		kind, err = mcptools.ValidateMediaType(out.Kind, kind, data)
		if err != nil {
			last = err
			continue
		}
		out.Data, out.ContentType = data, kind
		return out, nil
	}
	if result.NextCandidateOffset != nil {
		return mcptools.PublicShare{}, errors.New("share_media_candidates_partial")
	}
	if last != nil {
		return mcptools.PublicShare{}, last
	}
	return mcptools.PublicShare{}, errors.New("share_media_unavailable")
}
