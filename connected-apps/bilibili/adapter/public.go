// Package adapter exposes owner-approved anonymous Bilibili metadata and
// public observations. The host controls the BV scope and each permission.
package adapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/workspacefs"
)

const (
	// ID is the stable public application identifier used by apphost.
	ID = "bilibili"

	installationID = "bilibili-public"
	connectionID   = "public"
	accountID      = "anonymous"
	settingsName   = "bilibili-public-settings.json"

	metadataEndpoint      = "https://api.bilibili.com/x/web-interface/view"
	requestTimeout        = 12 * time.Second
	maxResponseBytes      = 1 << 20
	maxMediaBytes         = 32 << 20
	defaultPlaybackSample = 6 * time.Second
	minPlaybackSample     = 1 * time.Second
	maxPlaybackSample     = 20 * time.Second
)

var (
	// ErrConflict means a caller attempted to overwrite a newer owner setting.
	ErrConflict = errors.New("B站公开资料设置已更新，请重新读取后保存")
	// ErrInvalid means supplied settings or persisted settings are invalid.
	ErrInvalid = errors.New("B站公开资料设置无效")

	// Bilibili public BV identifiers are canonical, case-sensitive identifiers.
	// Do not accept URLs, av IDs, lower-cased IDs, or an identifier embedded in prose:
	// doing so would make an owner allowlist less exact than it appears.
	canonicalBV = regexp.MustCompile(`^BV[0-9A-Za-z]{10}$`)
)

// Settings is owner-controlled local policy. Videos is an exact, ordered
// allowlist; it is deliberately the only discovery source for this adapter.
type Settings struct {
	Revision        uint64   `json:"revision"`
	Enabled         bool     `json:"enabled"`
	Videos          []string `json:"videos"`
	BrowserEnabled  bool     `json:"browser_enabled"`
	PlaybackEnabled bool     `json:"playback_enabled"`
	CommentsEnabled bool     `json:"comments_enabled"`
	AudioEnabled    bool     `json:"audio_enabled"`
}

// PublicApp implements the apphost provider and observation provider for an
// anonymous public-metadata connection. No credential, cookie, or account
// state is accepted by this type.
type PublicApp struct {
	privateDirectory string
	client           *http.Client
	readPage         func(context.Context, string, func() error) (stationbrowser.Page, error)
	resolveMedia     func(context.Context, string, int64) (mcptools.PublicVideo, error)
	samplePlayback   func(context.Context, []byte, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error)
	browserAvailable func() bool
	audioVersion     func(context.Context) (string, error)
	transcribeAudio  func(context.Context, []byte, func() error) (string, error)
	// publicationClient is test-only injection for the publication workflow.
	// Production leaves it nil and constructs the direct Open API client.
	publicationClient        func(OpenCredentials) (publicationTransport, error)
	verifyPublicationAccount func(context.Context, OpenCredentials) (string, error)
	publicationUIKey         [32]byte
	publicationOAuth         publicationOAuthTransport
	publicationOAuthAttempt  *publicationOAuthAttempt
	mu                       sync.Mutex
	readGrant                context.Context
	cancelReads              context.CancelFunc
}

// New creates a public Bilibili application with private local policy storage.
// privateDirectory is owned by the station, not a Bilibili account.
func New(privateDirectory string) *PublicApp {
	reader := stationbrowser.New(filepath.Join(privateDirectory, "browser-profiles", connectionID))
	grant, cancel := context.WithCancel(context.Background())
	app := &PublicApp{
		privateDirectory: privateDirectory,
		readPage:         reader.Read,
		resolveMedia:     mcptools.GetPublicBilibiliVideo,
		samplePlayback:   reader.SamplePlayback,
		browserAvailable: stationbrowser.Available,
		readGrant:        grant,
		cancelReads:      cancel,
		client: &http.Client{
			Timeout: requestTimeout,
			Jar:     nil,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	_, _ = rand.Read(app.publicationUIKey[:])
	return app
}

func (p *PublicApp) BrowserAvailable() bool {
	return p != nil && p.browserAvailable != nil && p.browserAvailable()
}

// Registration supplies a fixed, anonymous, read-only installation.
func (p *PublicApp) Registration() apphost.Registration {
	return apphost.Registration{
		Application: apphost.Application{
			ID:             ID,
			Name:           "哔哩哔哩公开资料",
			InstallationID: installationID,
			Registration:   "public-metadata",
		},
		Provider: p,
	}
}

// View returns the local owner policy without contacting Bilibili.
func (p *PublicApp) View() (Settings, error) {
	if p == nil {
		return Settings{}, ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadLocked()
}

// Save writes an owner allowlist with compare-and-swap semantics. It never
// probes video IDs: saving must work while offline and must not leak browsing
// intentions to a provider.
func (p *PublicApp) Save(expected uint64, next Settings) (Settings, error) {
	if p == nil {
		return Settings{}, ErrInvalid
	}
	cleaned, err := normalizeVideos(next.Enabled, next.Videos)
	if err != nil {
		return Settings{}, err
	}
	if !next.validPermissions() {
		return Settings{}, ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	current, err := p.loadLocked()
	if err != nil {
		return Settings{}, err
	}
	if expected != current.Revision {
		return current, ErrConflict
	}
	if next.BrowserEnabled && !p.BrowserAvailable() {
		return current, apphost.ErrObservationBrowserUnavailable
	}
	next.Revision, next.Videos = current.Revision+1, cleaned
	if next.Revision == 0 { // defensive overflow handling
		return Settings{}, ErrInvalid
	}
	if err := p.saveLocked(next); err != nil {
		return Settings{}, err
	}
	p.cancelReads()
	p.readGrant, p.cancelReads = context.WithCancel(context.Background())
	return cloneSettings(next), nil
}

func (p *PublicApp) Connections(ctx context.Context) ([]apphost.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := p.View(); err != nil {
		return nil, apphost.ErrUnavailable
	}
	connections := []apphost.Connection{{
		ID: connectionID, AccountID: accountID, Label: "B站公开资料（匿名）", Primary: true, Configuration: "owner-allowlist",
	}}
	// Publication is deliberately opt-in.  A missing local credential file must
	// not turn the anonymous public reader into a configured publisher.
	if publication, configured := p.publicationConnection(); configured {
		connections = append(connections, publication)
	}
	return connections, nil
}

func (p *PublicApp) Inspect(ctx context.Context, id string) (apphost.ConnectionStatus, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ConnectionStatus{}, err
	}
	if id == publicationConnectionID {
		return p.inspectPublication(ctx)
	}
	if id != connectionID {
		return apphost.ConnectionStatus{}, apphost.ErrNotFound
	}
	settings, err := p.View()
	if err != nil {
		return apphost.ConnectionStatus{}, apphost.ErrUnavailable
	}
	connection := apphost.Connection{ID: connectionID, AccountID: accountID, Label: "B站公开资料（匿名）", Primary: true, Configuration: "owner-allowlist"}
	worker := "disabled"
	capability := "owner_allowlist_disabled"
	actions := []string{}
	if settings.Enabled {
		worker = "ready"
		capability = "ready"
		actions = []string{"content.observe"}
		if settings.AudioEnabled && p.AudioAvailable(ctx) {
			actions = append(actions, "audio.observe")
		}
		if settings.CommentsEnabled {
			actions = append(actions, "comments.observe")
		}
		if settings.BrowserEnabled && p.BrowserAvailable() {
			actions = append(actions, "page.observe")
			if settings.PlaybackEnabled {
				actions = append(actions, "playback.observe")
			}
		}
	}
	return apphost.ConnectionStatus{
		Connection: connection, Login: "anonymous", Runtime: "local", Worker: worker,
		CapabilityCheck: capability, SupportedActions: actions,
	}, nil
}

// ObservationTargets is local-only: it lists exact owner-approved IDs, and
// intentionally returns no remote title or description labels.
func (p *PublicApp) ObservationTargets(ctx context.Context, id string) ([]apphost.ObservationTarget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id != connectionID {
		return nil, apphost.ErrNotFound
	}
	settings, err := p.View()
	if err != nil {
		return nil, apphost.ErrUnavailable
	}
	if !settings.Enabled {
		return []apphost.ObservationTarget{}, nil
	}
	result := make([]apphost.ObservationTarget, 0, len(settings.Videos))
	fields := []apphost.ObservationField{apphost.ObservationTitle, apphost.ObservationBody}
	if settings.AudioEnabled && p.AudioAvailable(ctx) {
		fields = append(fields, apphost.ObservationAudio)
	}
	if settings.CommentsEnabled {
		fields = append(fields, apphost.ObservationComments)
	}
	if settings.BrowserEnabled && p.BrowserAvailable() {
		fields = append(fields, apphost.ObservationPageText)
		if settings.PlaybackEnabled {
			fields = append(fields, apphost.ObservationPlayback)
		}
	}
	for _, video := range settings.Videos {
		result = append(result, apphost.ObservationTarget{
			ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: connectionID, AccountID: accountID},
			ContentID:     video,
			Fields:        fields,
		})
	}
	return result, nil
}

// ObservationGrant checks the exact current policy. apphost repeats this
// check during a read and again before downstream use of the evidence.
func (p *PublicApp) ObservationGrant(ctx context.Context, access apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ObservationGrant{}, err
	}
	settings, err := p.View()
	if err != nil {
		return apphost.ObservationGrant{}, apphost.ErrUnavailable
	}
	if !p.allowed(settings, access) {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	version := "bilibili-public-" + strconv.FormatUint(settings.Revision, 10)
	if access.Field == apphost.ObservationAudio {
		asr, err := p.localAudioVersion(ctx)
		if err != nil {
			return apphost.ObservationGrant{}, err
		}
		version += "-" + asr
	}
	return apphost.ObservationGrant{Allowed: true, Version: version}, nil
}

// ReadObservation reads one exact field from the current owner allowlist.
func (p *PublicApp) ReadObservation(ctx context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ObservationPage{}, err
	}
	if request.Limit < 0 || (request.Field != apphost.ObservationComments && request.Field != apphost.ObservationAudio && request.Cursor != "") {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	settings, err := p.View()
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrUnavailable
	}
	if !p.allowed(settings, request.ObservationAccess) {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	if request.Field == apphost.ObservationPageText {
		return p.readBrowserObservation(ctx, request.ObservationAccess, settings.Revision)
	}
	if request.Field == apphost.ObservationPlayback {
		return p.readPlaybackObservation(ctx, request.ObservationAccess, settings.Revision, request.Limit)
	}
	if request.Field == apphost.ObservationAudio {
		return p.readAudio(ctx, request, settings.Revision)
	}
	if request.Field == apphost.ObservationComments {
		return p.readComments(ctx, request, settings.Revision)
	}
	data, err := p.fetch(ctx, request.ContentID)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	switch request.Field {
	case apphost.ObservationTitle:
		if strings.TrimSpace(data.Title) == "" {
			return apphost.ObservationPage{}, apphost.ErrObservationContentUnavailable
		}
		return apphost.ObservationPage{Chunks: []apphost.ObservationChunk{{Position: 0, Text: data.Title}}, Coverage: "B站公开资料：视频标题原文", Complete: true}, nil
	case apphost.ObservationBody:
		if strings.TrimSpace(data.Description) == "" {
			return apphost.ObservationPage{Coverage: "B站公开资料：投稿者简介原文为空", Complete: true}, nil
		}
		return apphost.ObservationPage{Chunks: []apphost.ObservationChunk{{Position: 0, Text: data.Description}}, Coverage: "B站公开资料：投稿者简介原文", Complete: true}, nil
	default:
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
}

func (p *PublicApp) allowed(settings Settings, access apphost.ObservationAccess) bool {
	if !settings.Enabled || access.AppID != ID || access.InstallationID != installationID || access.ConnectionID != connectionID || access.AccountID != accountID ||
		!canonicalBV.MatchString(access.ContentID) {
		return false
	}
	switch access.Field {
	case apphost.ObservationTitle, apphost.ObservationBody:
	case apphost.ObservationAudio:
		if !settings.AudioEnabled {
			return false
		}
	case apphost.ObservationComments:
		if !settings.CommentsEnabled {
			return false
		}
	case apphost.ObservationPageText:
		if !settings.BrowserEnabled {
			return false
		}
	case apphost.ObservationPlayback:
		if !settings.BrowserEnabled || !settings.PlaybackEnabled {
			return false
		}
	default:
		return false
	}
	for _, video := range settings.Videos {
		if video == access.ContentID {
			return true
		}
	}
	return false
}

func (p *PublicApp) readBrowserObservation(ctx context.Context, access apphost.ObservationAccess, revision uint64) (apphost.ObservationPage, error) {
	if !p.BrowserAvailable() {
		return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
	}
	readCtx, authorize, closeScope := p.observationReadScope(ctx, access, revision)
	defer closeScope()
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	page, err := p.readPage(readCtx, "https://www.bilibili.com/video/"+access.ContentID+"/", authorize)
	if authErr := authorize(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if errors.Is(err, stationbrowser.ErrPageUnavailable) {
		return apphost.ObservationPage{}, apphost.ErrObservationContentUnavailable
	}
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
	}
	text := "页面标题：" + page.Title
	if page.Description != "" {
		text += "\n页面简介：" + page.Description
	}
	return apphost.ObservationPage{
		Chunks:   []apphost.ObservationChunk{{Position: 0, Text: text}},
		Coverage: "独立 Edge 页面：标题与可取得的简介",
		Complete: true,
	}, nil
}

func (p *PublicApp) observationReadScope(ctx context.Context, access apphost.ObservationAccess, revision uint64) (context.Context, func() error, func()) {
	p.mu.Lock()
	grant := p.readGrant
	p.mu.Unlock()
	readCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(grant, cancel)
	authorize := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := p.View()
		if err != nil {
			return apphost.ErrUnavailable
		}
		if grant.Err() != nil || current.Revision != revision || !p.allowed(current, access) {
			return apphost.ErrObservationForbidden
		}
		return nil
	}
	return readCtx, authorize, func() {
		stop()
		cancel()
	}
}

// readPlaybackObservation measures a bounded interval in a fresh, private
// Edge instance. The adapter passes media bytes only. It never gives the
// browser a provider URL, and never persists or returns the CDN URL.
func (p *PublicApp) readPlaybackObservation(ctx context.Context, access apphost.ObservationAccess, revision uint64, limit int) (apphost.ObservationPage, error) {
	if !p.BrowserAvailable() || p.resolveMedia == nil || p.samplePlayback == nil {
		return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
	}
	sample, err := playbackSampleLimit(limit)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	readCtx, authorize, closeScope := p.observationReadScope(ctx, access, revision)
	defer closeScope()
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	media, err := p.resolveMedia(readCtx, access.ContentID, maxMediaBytes)
	if authErr := authorize(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return apphost.ObservationPage{}, ctx.Err()
		}
		return apphost.ObservationPage{}, classifyPlaybackError(err)
	}
	if len(media.Data) > maxMediaBytes {
		return apphost.ObservationPage{}, apphost.ErrObservationPlaybackSize
	}
	if media.LinkOnly || len(media.Data) == 0 || media.DurationMS <= 0 || media.ContentType == "" {
		return apphost.ObservationPage{}, apphost.ErrObservationPlaybackUnavailable
	}
	result, err := p.samplePlayback(readCtx, media.Data, media.ContentType, sample, authorize)
	if authErr := authorize(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return apphost.ObservationPage{}, ctx.Err()
		}
		if errors.Is(err, apphost.ErrObservationForbidden) {
			return apphost.ObservationPage{}, err
		}
		if errors.Is(err, stationbrowser.ErrBrowserUnavailable) {
			return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
		}
		return apphost.ObservationPage{}, apphost.ErrObservationPlaybackUnavailable
	}
	intervals := make([]apphost.PlaybackInterval, 0, len(result.Intervals))
	for _, interval := range result.Intervals {
		intervals = append(intervals, apphost.PlaybackInterval{StartSeconds: interval.StartSeconds, EndSeconds: interval.EndSeconds})
	}
	return apphost.ObservationPage{
		Playback: intervals,
		PlaybackProof: &apphost.PlaybackProof{
			RequestedSeconds:     int(sample / time.Second),
			MediaDurationSeconds: result.DurationSeconds,
			Ready:                result.Ready,
			Ended:                result.Ended,
			Paused:               result.Paused,
		},
		Coverage: "独立 Edge 实际播放采样",
		Complete: true,
	}, nil
}

func playbackSampleLimit(limit int) (time.Duration, error) {
	if limit == 0 {
		return defaultPlaybackSample, nil
	}
	if limit < int(minPlaybackSample/time.Second) || limit > int(maxPlaybackSample/time.Second) {
		return 0, apphost.ErrObservationPlaybackLimit
	}
	return time.Duration(limit) * time.Second, nil
}

func classifyPlaybackError(err error) error {
	var web *mcptools.WebError
	if errors.As(err, &web) {
		switch {
		case web.Status == http.StatusTooManyRequests || web.Status == http.StatusPreconditionFailed:
			return apphost.ErrObservationThrottled
		case web.Code == "response_too_large":
			return apphost.ErrObservationPlaybackSize
		case web.Status >= http.StatusInternalServerError || web.Code == "network_timeout" || web.Code == "network_unavailable" || web.Code == "network_closed" || web.Code == "dns_failed" || web.Code == "tls_failed":
			return apphost.ErrObservationUpstreamUnavailable
		}
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return apphost.ErrObservationUpstreamUnavailable
	}
	return apphost.ErrObservationPlaybackUnavailable
}

type metadataResponse struct {
	Code *int `json:"code"`
	Data struct {
		BVID  string `json:"bvid"`
		AID   int64  `json:"aid"`
		Title string `json:"title"`
		Desc  string `json:"desc"`
	} `json:"data"`
}

type publicMetadata struct {
	AID         int64
	Title       string
	Description string
}

func (p *PublicApp) fetch(ctx context.Context, bvid string) (publicMetadata, error) {
	var decoded metadataResponse
	if err := p.fetchJSON(ctx, metadataEndpoint, url.Values{"bvid": {bvid}}, &decoded); err != nil {
		return publicMetadata{}, err
	}
	if err := publicResponseCode(decoded.Code); err != nil {
		return publicMetadata{}, err
	}
	if decoded.Data.BVID != bvid {
		return publicMetadata{}, apphost.ErrObservationContentUnavailable
	}
	return publicMetadata{AID: decoded.Data.AID, Title: decoded.Data.Title, Description: decoded.Data.Desc}, nil
}

func (p *PublicApp) fetchJSON(ctx context.Context, endpoint string, query url.Values, destination any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.client == nil {
		return apphost.ErrObservationUpstreamUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return apphost.ErrObservationUpstreamUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Agent-home-station/1.0 public-metadata")
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return apphost.ErrObservationUpstreamUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusPreconditionFailed, http.StatusTooManyRequests:
		return apphost.ErrObservationThrottled
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return apphost.ErrObservationContentUnavailable
	case http.StatusOK:
	default:
		return apphost.ErrObservationUpstreamUnavailable
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(encoded) > maxResponseBytes || json.Unmarshal(encoded, destination) != nil {
		return apphost.ErrObservationUpstreamUnavailable
	}
	return nil
}

func publicResponseCode(code *int) error {
	if code != nil && (*code == -412 || *code == -352 || *code == -429) {
		return apphost.ErrObservationThrottled
	}
	if code == nil || *code != 0 {
		return apphost.ErrObservationContentUnavailable
	}
	return nil
}

func (s Settings) validPermissions() bool {
	return (s.Enabled || !s.BrowserEnabled && !s.CommentsEnabled && !s.AudioEnabled) && (!s.PlaybackEnabled || s.BrowserEnabled)
}

func normalizeVideos(enabled bool, videos []string) ([]string, error) {
	if len(videos) > 100 {
		return nil, ErrInvalid
	}
	if enabled && len(videos) == 0 {
		return nil, ErrInvalid
	}
	result := make([]string, 0, len(videos))
	seen := make(map[string]struct{}, len(videos))
	for _, video := range videos {
		if !canonicalBV.MatchString(video) {
			return nil, ErrInvalid
		}
		if _, exists := seen[video]; exists {
			return nil, ErrInvalid
		}
		seen[video] = struct{}{}
		result = append(result, video)
	}
	return result, nil
}

func (p *PublicApp) loadLocked() (Settings, error) {
	if strings.TrimSpace(p.privateDirectory) == "" {
		return Settings{}, ErrInvalid
	}
	root, err := workspacefs.EnsurePath(p.privateDirectory)
	if err != nil {
		return Settings{}, apphost.ErrUnavailable
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, settingsName)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{Revision: 1, Videos: []string{}}, nil
	}
	if err != nil {
		return Settings{}, apphost.ErrUnavailable
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 256*1024+1))
	if err != nil || len(encoded) > 256*1024 {
		return Settings{}, apphost.ErrUnavailable
	}
	var settings Settings
	if json.Unmarshal(encoded, &settings) != nil || settings.Revision == 0 || !settings.validPermissions() {
		return Settings{}, apphost.ErrUnavailable
	}
	videos, err := normalizeVideos(settings.Enabled, settings.Videos)
	if err != nil {
		return Settings{}, apphost.ErrUnavailable
	}
	settings.Videos = videos
	return cloneSettings(settings), nil
}

func (p *PublicApp) saveLocked(settings Settings) error {
	return p.savePrivateJSON(settingsName, settings)
}

func (p *PublicApp) savePrivateJSON(name string, value any) error {
	root, err := workspacefs.EnsurePath(p.privateDirectory)
	if err != nil {
		return apphost.ErrUnavailable
	}
	defer root.Close()
	encoded, err := json.Marshal(value)
	if err != nil {
		return apphost.ErrUnavailable
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		return apphost.ErrUnavailable
	}
	temporary := "." + name + "." + hex.EncodeToString(random[:]) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return apphost.ErrUnavailable
	}
	defer func() { _ = root.Remove(temporary) }()
	if _, err = file.Write(encoded); err != nil {
		_ = file.Close()
		return apphost.ErrUnavailable
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return apphost.ErrUnavailable
	}
	if err = file.Close(); err != nil {
		return apphost.ErrUnavailable
	}
	if err = root.Rename(temporary, name); err != nil {
		return apphost.ErrUnavailable
	}
	return nil
}

func cloneSettings(settings Settings) Settings {
	settings.Videos = append([]string(nil), settings.Videos...)
	if settings.Videos == nil {
		settings.Videos = []string{}
	}
	return settings
}
