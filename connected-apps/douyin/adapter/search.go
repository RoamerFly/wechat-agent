// Package adapter connects approved Douyin accounts and reads authorized content through the official platform.
package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/workspacefs"
)

const (
	// ID is the stable application identifier used by apphost.
	ID = "douyin"

	installationID = "douyin-search"
	connectionID   = "search"
	accountID      = "official-client-token"
	settingsName   = "search-settings.json"

	searchEndpoint   = "https://open.douyin.com/dy_open_api/v1/search/video/"
	requestTimeout   = 12 * time.Second
	maxResponseBytes = 1 << 20
	maxSettingsBytes = 64 << 10
	defaultPageSize  = 10
	maxPageSize      = 50
)

var (
	// ErrInvalid reports malformed local owner configuration. It intentionally
	// contains no filesystem path, token, or device identifier.
	ErrInvalid      = errors.New("抖音搜索设置无效")
	errUnconfigured = errors.New("抖音搜索尚未配置")
	decimalID       = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	clientToken     = regexp.MustCompile(`^clt[.][A-Za-z0-9_-]{1,512}$`)
)

// SearchView is the owner-visible, sanitized configuration state. Credentials
// and device identifiers must never be presented by View or apphost status.
type SearchView struct {
	Configured bool      `json:"configured"`
	Enabled    bool      `json:"enabled"`
	Status     string    `json:"status"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Keywords   []string  `json:"keywords"`
}

// settings is local owner policy. Its JSON shape is intentionally small and
// fixed: the client token is an application credential, not a user account.
type settings struct {
	SchemaVersion        int      `json:"schema_version"`
	Enabled              bool     `json:"enabled"`
	ClientToken          string   `json:"client_token"`
	ExpiresAt            string   `json:"expires_at"`
	DeviceID             string   `json:"device_id"`
	Keywords             []string `json:"keywords"`
	FollowPublicVideos   bool     `json:"follow_public_videos,omitempty"`
	SamplePublicPlayback bool     `json:"sample_public_playback,omitempty"`
}

type loadedSettings struct {
	settings
	expiresAt time.Time
	version   string
}

// SearchApp implements separately authorized official account and content connections.
type SearchApp struct {
	privateDirectory string
	client           *http.Client
	now              func() time.Time
	mu               sync.Mutex
	accountMu        sync.Mutex
	accountRefreshMu sync.Mutex
	accountPending   *accountAttempt
	// webRead is installed by the trusted station before the app starts. The
	// adapter never receives the browser profile, cookies, grant version, or
	// pagination state behind this callback.
	webRead      WebReadConnection
	samplePlayer func(context.Context, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error)
}

// New constructs the adapter. The supplied directory is owned by the station
// and is used solely to read owner-local settings through workspacefs.
func New(privateDirectory string) *SearchApp {
	return &SearchApp{
		privateDirectory: privateDirectory,
		samplePlayer:     stationbrowser.New(filepath.Join(privateDirectory, "browser-profiles", "official-player")).SampleDouyinPlayer,
		now:              time.Now,
		client: &http.Client{
			Timeout:       requestTimeout,
			Jar:           nil,
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (p *SearchApp) Registration() apphost.Registration {
	return apphost.Registration{Application: apphost.Application{
		ID: ID, Name: "抖音", InstallationID: installationID, Registration: "official-read-v1",
	}, Provider: p}
}

// View reads local policy only. It never contacts Douyin and never exposes a
// token or device id.
func (p *SearchApp) View() (SearchView, error) {
	loaded, err := p.load()
	if err != nil {
		return SearchView{}, err
	}
	view := SearchView{Configured: true, Enabled: loaded.Enabled, ExpiresAt: loaded.expiresAt, Keywords: append([]string(nil), loaded.Keywords...)}
	switch {
	case !loaded.Enabled:
		view.Status = "disabled"
	case !p.nowUTC().Before(loaded.expiresAt):
		view.Status = "expired"
	default:
		view.Status = "ready"
	}
	return view, nil
}

func (p *SearchApp) Connections(ctx context.Context) ([]apphost.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A static connection lets the UI show configuration and expiry problems
	// without treating a not-yet-configured app as absent.
	connections := []apphost.Connection{p.searchConnection(), p.ownVideoConnection(), p.accountConnection()}
	connection, present, err := p.webReadConnection(ctx)
	if err != nil {
		return nil, err
	}
	if present {
		connections = append(connections, connection)
	}
	return connections, nil
}

func (p *SearchApp) searchConnection() apphost.Connection {
	return apphost.Connection{ID: connectionID, AccountID: accountID, Label: "抖音官方视频搜索", Primary: true, Configuration: p.connectionState()}
}

func (p *SearchApp) Inspect(ctx context.Context, id string) (apphost.ConnectionStatus, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ConnectionStatus{}, err
	}
	var connection apphost.Connection
	var action string
	switch id {
	case WebReadConnectionID:
		connection, present, err := p.webReadConnection(ctx)
		if err != nil {
			return apphost.ConnectionStatus{}, err
		}
		if !present {
			return apphost.ConnectionStatus{}, apphost.ErrNotFound
		}
		state := connection.Configuration
		status := apphost.ConnectionStatus{Connection: connection, Login: "saved网页登录资料", Runtime: "local", Worker: "disabled", CapabilityCheck: state}
		if state == "ready" {
			status.Worker = "ready"
			status.SupportedActions = []string{"content.page-text"}
		}
		return status, nil
	case connectionID:
		connection, action = p.searchConnection(), "content.search"
	case ownVideoConnectionID:
		connection, action = p.ownVideoConnection(), "content.own-video"
	case accountConnectionID:
		connection = p.accountConnection()
		if _, err := p.worksAccount(); err == nil {
			action = "content.account-works"
		}
	default:
		return apphost.ConnectionStatus{}, apphost.ErrNotFound
	}
	state := connection.Configuration
	status := apphost.ConnectionStatus{Connection: connection, Runtime: "local", Worker: "disabled", Login: state, CapabilityCheck: state}
	if state == "ready" {
		status.Worker = "ready"
		if action != "" {
			status.SupportedActions = []string{action}
		}
		if id == accountConnectionID {
			ref := apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: connection.ID, AccountID: connection.AccountID}
			if _, err := p.publicationAccount(ref); err == nil {
				status.SupportedActions = append(status.SupportedActions, "content.publish-video")
			}
		}
	}
	return status, nil
}

func (p *SearchApp) connectionState() string {
	if p == nil || strings.TrimSpace(p.privateDirectory) == "" {
		return "unconfigured"
	}
	view, err := p.View()
	if err != nil {
		if errors.Is(err, errUnconfigured) {
			return "unconfigured"
		}
		return "invalid"
	}
	return view.Status
}

func (p *SearchApp) ObservationTargets(ctx context.Context, id string) ([]apphost.ObservationTarget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == WebReadConnectionID {
		return p.webReadTargets(ctx)
	}
	if id == ownVideoConnectionID {
		return p.ownVideoTargets(ctx)
	}
	if id == accountConnectionID {
		return p.accountWorksTargets()
	}
	if id != connectionID {
		return nil, apphost.ErrNotFound
	}
	loaded, err := p.load()
	if err != nil || !loaded.Enabled || !p.nowUTC().Before(loaded.expiresAt) {
		return []apphost.ObservationTarget{}, nil
	}
	result := make([]apphost.ObservationTarget, 0, len(loaded.Keywords))
	for _, keyword := range loaded.Keywords {
		result = append(result, apphost.ObservationTarget{
			ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: connectionID, AccountID: accountID},
			ContentID:     keywordContentID(keyword), Label: keyword, Fields: []apphost.ObservationField{apphost.ObservationSearch},
		})
	}
	return result, nil
}

func (p *SearchApp) ObservationGrant(ctx context.Context, access apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ObservationGrant{}, err
	}
	if access.ConnectionID == WebReadConnectionID {
		valid, err := p.webReadAccess(ctx, access)
		if err != nil {
			return apphost.ObservationGrant{Allowed: false}, err
		}
		if !valid {
			return apphost.ObservationGrant{Allowed: false}, nil
		}
		return p.webRead.Grant(ctx, access)
	}
	if access.ConnectionID == ownVideoConnectionID {
		return p.ownVideoGrant(ctx, access)
	}
	if access.ConnectionID == accountConnectionID {
		return p.accountWorksGrant(access)
	}
	loaded, err := p.load()
	if err != nil {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	if !p.allowed(loaded, access) {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	if !p.nowUTC().Before(loaded.expiresAt) {
		return apphost.ObservationGrant{}, apphost.ErrObservationCredentialExpired
	}
	return apphost.ObservationGrant{Allowed: true, Version: loaded.version, Fresh: access.Field == apphost.ObservationPlayback}, nil
}

func (p *SearchApp) ReadObservation(ctx context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ObservationPage{}, err
	}
	if request.ConnectionID == WebReadConnectionID {
		valid, err := p.webReadAccess(ctx, request.ObservationAccess)
		if err != nil {
			return apphost.ObservationPage{}, err
		}
		if !valid {
			return apphost.ObservationPage{}, apphost.ErrObservationForbidden
		}
		return p.webRead.Read(ctx, request)
	}
	if request.ConnectionID == ownVideoConnectionID {
		return p.readOwnVideo(ctx, request)
	}
	if request.ConnectionID == accountConnectionID {
		return p.readAccountWorks(ctx, request)
	}
	switch request.Field {
	case apphost.ObservationPlayerEntry:
		return p.readPlayerEntry(ctx, request)
	case apphost.ObservationPlayback:
		return p.readPlayerPlayback(ctx, request)
	}
	if request.Field != apphost.ObservationSearch || request.Limit < 0 {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	loaded, err := p.load()
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	if !p.allowed(loaded, request.ObservationAccess) {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	if !p.nowUTC().Before(loaded.expiresAt) {
		return apphost.ObservationPage{}, apphost.ErrObservationCredentialExpired
	}
	cursor, err := parseCursor(request, loaded)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	if err := p.authorize(loaded, request.ObservationAccess); err != nil {
		return apphost.ObservationPage{}, err
	}
	response, err := p.search(ctx, loaded, cursor)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	// Changes, revocations, and expiry while a request was in flight invalidate
	// the page before it can be cached or used by a caller.
	if err := p.authorize(loaded, request.ObservationAccess); err != nil {
		return apphost.ObservationPage{}, err
	}
	page, err := buildPage(response, cursor)
	if err != nil || !loaded.FollowPublicVideos {
		return page, err
	}
	for i := range page.Chunks {
		chunk := &page.Chunks[i]
		if chunk.SourceURL != "" {
			chunk.FollowUpField = apphost.ObservationPlayerEntry
			chunk.FollowUpContentID = playerFollowupID(loaded, cursor.ContentID, chunk.SourceID)
		}
	}
	return page, nil
}

func (p *SearchApp) authorize(previous loadedSettings, access apphost.ObservationAccess) error {
	current, err := p.load()
	if err != nil || current.version != previous.version || !p.allowed(current, access) {
		return apphost.ErrObservationForbidden
	}
	if !p.nowUTC().Before(current.expiresAt) {
		return apphost.ErrObservationCredentialExpired
	}
	return nil
}

func (p *SearchApp) allowed(loaded loadedSettings, access apphost.ObservationAccess) bool {
	if !loaded.Enabled || access.AppID != ID || access.InstallationID != installationID || access.ConnectionID != connectionID || access.AccountID != accountID {
		return false
	}
	switch access.Field {
	case apphost.ObservationPlayerEntry:
		return playerFollowupVideo(loaded, access.ContentID) != ""
	case apphost.ObservationPlayback:
		return loaded.SamplePublicPlayback && playerFollowupVideo(loaded, access.ContentID) != ""
	}
	if access.Field != apphost.ObservationSearch {
		return false
	}
	for _, keyword := range loaded.Keywords {
		if access.ContentID == keywordContentID(keyword) {
			return true
		}
	}
	return false
}

func (p *SearchApp) nowUTC() time.Time {
	if p == nil || p.now == nil {
		return time.Now().UTC()
	}
	return p.now().UTC()
}

func (p *SearchApp) load() (loadedSettings, error) {
	if p == nil || strings.TrimSpace(p.privateDirectory) == "" {
		return loadedSettings{}, ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	root, err := workspacefs.OpenPath(p.privateDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loadedSettings{}, errUnconfigured
		}
		return loadedSettings{}, ErrInvalid
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, settingsName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loadedSettings{}, errUnconfigured
		}
		return loadedSettings{}, ErrInvalid
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil || len(raw) > maxSettingsBytes {
		return loadedSettings{}, ErrInvalid
	}
	var decoded settings
	if json.Unmarshal(raw, &decoded) != nil {
		return loadedSettings{}, ErrInvalid
	}
	expiresAt, err := validateSettings(decoded)
	if err != nil {
		return loadedSettings{}, ErrInvalid
	}
	digest := sha256.Sum256(raw)
	return loadedSettings{settings: decoded, expiresAt: expiresAt, version: "douyin-search-" + hex.EncodeToString(digest[:])}, nil
}

func validateSettings(s settings) (time.Time, error) {
	if s.SchemaVersion != 1 || !clientToken.MatchString(s.ClientToken) || !decimalID.MatchString(s.DeviceID) || len(s.Keywords) == 0 || len(s.Keywords) > 50 || s.SamplePublicPlayback && !s.FollowPublicVideos {
		return time.Time{}, ErrInvalid
	}
	if deviceID, err := strconv.ParseInt(s.DeviceID, 10, 64); err != nil || deviceID <= 0 {
		return time.Time{}, ErrInvalid
	}
	expiresAt, err := time.Parse(time.RFC3339, s.ExpiresAt)
	if err != nil || expiresAt.IsZero() {
		return time.Time{}, ErrInvalid
	}
	seen := make(map[string]struct{}, len(s.Keywords))
	for _, keyword := range s.Keywords {
		if keyword == "" || strings.TrimSpace(keyword) != keyword || len([]rune(keyword)) > 100 {
			return time.Time{}, ErrInvalid
		}
		if _, duplicate := seen[keyword]; duplicate {
			return time.Time{}, ErrInvalid
		}
		seen[keyword] = struct{}{}
	}
	return expiresAt.UTC(), nil
}

func keywordContentID(keyword string) string {
	digest := sha256.Sum256([]byte("douyin-search-keyword-v1\x00" + keyword))
	return "search-" + hex.EncodeToString(digest[:16])
}

type searchCursor struct {
	ContentID string `json:"content_id"`
	Cursor    int64  `json:"cursor"`
	SearchID  string `json:"search_id,omitempty"`
	Position  int    `json:"position"`
	PageSize  int    `json:"page_size"`
}

func parseCursor(request apphost.ObservationRequest, loaded loadedSettings) (searchCursor, error) {
	keyword := ""
	for _, candidate := range loaded.Keywords {
		if request.ContentID == keywordContentID(candidate) {
			keyword = candidate
			break
		}
	}
	if keyword == "" {
		return searchCursor{}, apphost.ErrObservationForbidden
	}
	if request.Cursor == "" {
		size := request.Limit
		if size == 0 {
			size = defaultPageSize
		}
		if size < 1 || size > maxPageSize {
			return searchCursor{}, apphost.ErrObservationSearchPageSize
		}
		return searchCursor{ContentID: request.ContentID, PageSize: size}, nil
	}
	encoded, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil {
		return searchCursor{}, apphost.ErrObservationInvalid
	}
	var cursor searchCursor
	if json.Unmarshal(encoded, &cursor) != nil || cursor.ContentID != request.ContentID || cursor.Cursor < 0 || cursor.Position < 0 || cursor.PageSize < 1 || cursor.PageSize > maxPageSize || strings.TrimSpace(cursor.SearchID) == "" {
		return searchCursor{}, apphost.ErrObservationInvalid
	}
	if request.Limit != 0 && request.Limit != cursor.PageSize {
		return searchCursor{}, apphost.ErrObservationSearchPageSize
	}
	return cursor, nil
}

type searchResponse struct {
	ErrNo  *int   `json:"err_no"`
	ErrMsg string `json:"err_msg"`
	Data   *struct {
		Data *struct {
			Cursor    int64         `json:"cursor"`
			HasMore   *bool         `json:"has_more"`
			SearchID  string        `json:"search_id"`
			VideoList []searchVideo `json:"video_list"`
		} `json:"data"`
	} `json:"data"`
}

type searchVideo struct {
	ItemID     json.RawMessage `json:"item_id"`
	Title      string          `json:"title"`
	CreateTime int64           `json:"create_time"`
	Link       string          `json:"link"`
}

func (p *SearchApp) search(ctx context.Context, loaded loadedSettings, cursor searchCursor) (searchResponse, error) {
	if p == nil || p.client == nil {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	keyword := ""
	for _, candidate := range loaded.Keywords {
		if cursor.ContentID == keywordContentID(candidate) {
			keyword = candidate
			break
		}
	}
	if keyword == "" {
		return searchResponse{}, apphost.ErrObservationForbidden
	}
	query := url.Values{
		"count": {strconv.Itoa(cursor.PageSize)}, "device_id": {loaded.DeviceID}, "keyword": {keyword}, "cursor": {strconv.FormatInt(cursor.Cursor, 10)},
	}
	if cursor.SearchID != "" {
		query.Set("search_id", cursor.SearchID)
	}
	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, searchEndpoint+"?"+query.Encode(), nil)
	if err != nil {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Agent-home-station/1.0 douyin-search")
	req.Header.Set("access-token", loaded.ClientToken)
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return searchResponse{}, ctx.Err()
		}
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusPreconditionFailed {
		return searchResponse{}, apphost.ErrObservationThrottled
	}
	if response.StatusCode != http.StatusOK {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	var decoded searchResponse
	if json.Unmarshal(body, &decoded) != nil {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	if decoded.ErrNo == nil {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	if err := searchResponseError(*decoded.ErrNo); err != nil {
		return searchResponse{}, err
	}
	if decoded.Data == nil || decoded.Data.Data == nil {
		return searchResponse{}, apphost.ErrObservationUpstreamUnavailable
	}
	return decoded, nil
}

func searchResponseError(code int) error {
	switch code {
	case 0:
		return nil
	case 28001003, 28001008:
		return apphost.ErrObservationCredentialExpired
	case 28001014, 28001016, 28001018, 28001019:
		return apphost.ErrObservationCapabilityDenied
	case 28003017:
		return apphost.ErrObservationThrottled
	case 28001005, 28001006:
		return apphost.ErrObservationUpstreamUnavailable
	default:
		return apphost.ErrObservationContentUnavailable
	}
}

func buildPage(response searchResponse, cursor searchCursor) (apphost.ObservationPage, error) {
	if response.Data == nil || response.Data.Data == nil {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	data := response.Data.Data
	if data.HasMore == nil || len(data.VideoList) > cursor.PageSize || data.Cursor < 0 {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	searchID := cursor.SearchID
	if searchID == "" {
		searchID = strings.TrimSpace(data.SearchID)
	} else if incoming := strings.TrimSpace(data.SearchID); incoming != "" && incoming != searchID {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	if *data.HasMore && searchID == "" {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	page := apphost.ObservationPage{Complete: !*data.HasMore, Coverage: fmt.Sprintf("抖音开放平台视频搜索 · 已核准关键词 · 每页请求 %d 条", cursor.PageSize)}
	seen := make(map[string]struct{}, len(data.VideoList))
	untitled := 0
	for _, video := range data.VideoList {
		sourceID, ok := canonicalItemID(video.ItemID)
		if !ok || video.CreateTime < 0 || video.CreateTime > 253402300799 {
			return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
		}
		if _, duplicate := seen[sourceID]; duplicate {
			return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
		}
		seen[sourceID] = struct{}{}
		position := cursor.Position
		cursor.Position++
		if strings.TrimSpace(video.Title) == "" {
			untitled++
			continue
		}
		chunk := apphost.ObservationChunk{Position: position, Text: video.Title, SourceID: sourceID}
		if video.CreateTime > 0 {
			chunk.PublishedAt = time.Unix(video.CreateTime, 0).UTC().Format(time.RFC3339)
		}
		canonical := "https://www.douyin.com/video/" + sourceID
		if video.Link == canonical || video.Link == canonical+"/" {
			chunk.SourceURL = video.Link
		}
		page.Chunks = append(page.Chunks, chunk)
	}
	if untitled > 0 {
		page.Coverage += fmt.Sprintf(" · %d 条未提供标题", untitled)
	}
	if *data.HasMore {
		if data.Cursor <= cursor.Cursor {
			return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
		}
		cursor.Cursor = data.Cursor
		cursor.SearchID = searchID
		raw, err := json.Marshal(cursor)
		if err != nil {
			return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func canonicalItemID(raw json.RawMessage) (string, bool) {
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		value := number.String()
		return value, decimalID.MatchString(value)
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || !decimalID.MatchString(value) {
		return "", false
	}
	return value, true
}

var _ apphost.Provider = (*SearchApp)(nil)
var _ apphost.ObservationProvider = (*SearchApp)(nil)
