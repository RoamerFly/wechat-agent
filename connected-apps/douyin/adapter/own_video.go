package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/workspacefs"
)

const (
	ownVideoConnectionID = "own-videos"
	ownVideoSettingsName = "own-videos-settings.json"
	ownVideoEndpoint     = "https://open.douyin.com/api/douyin/v1/video/video_basic_info/"
)

var (
	errOwnVideoUnconfigured = errors.New("抖音本人作品观察尚未配置")
	accessToken             = regexp.MustCompile(`^act[.][A-Za-z0-9_-]{1,512}$`)
	openID                  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
)

// ownVideoSettings is owner-local policy for the documented posting.behavior
// endpoint.  It is deliberately independent of public search credentials.
type ownVideoSettings struct {
	SchemaVersion int      `json:"schema_version"`
	Enabled       bool     `json:"enabled"`
	Scope         string   `json:"scope"`
	AccessToken   string   `json:"access_token"`
	OpenID        string   `json:"open_id"`
	ExpiresAt     string   `json:"expires_at"`
	VideoIDs      []string `json:"video_ids"`
}

type loadedOwnVideoSettings struct {
	ownVideoSettings
	expiresAt time.Time
	version   string
}

func (p *SearchApp) ownVideoConnection() apphost.Connection {
	connection := apphost.Connection{ID: ownVideoConnectionID, Label: "抖音本人作品状态", Configuration: "unconfigured"}
	if p == nil || strings.TrimSpace(p.privateDirectory) == "" {
		return connection
	}
	loaded, err := p.loadOwnVideo()
	if err != nil {
		if !errors.Is(err, errOwnVideoUnconfigured) {
			connection.Configuration = "invalid"
		}
		return connection
	}
	connection.AccountID = ownVideoAccountID(loaded.OpenID)
	switch {
	case !loaded.Enabled:
		connection.Configuration = "disabled"
	case !p.nowUTC().Before(loaded.expiresAt):
		connection.Configuration = "expired"
	default:
		connection.Configuration = "ready"
	}
	return connection
}

func ownVideoAccountID(value string) string {
	digest := sha256.Sum256([]byte("douyin-own-video-account-v1\x00" + value))
	return "authorized-" + hex.EncodeToString(digest[:16])
}

func (p *SearchApp) ownVideoTargets(ctx context.Context) ([]apphost.ObservationTarget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loaded, err := p.loadOwnVideo()
	if err != nil || !loaded.Enabled || !p.nowUTC().Before(loaded.expiresAt) {
		return []apphost.ObservationTarget{}, nil
	}
	accountID := ownVideoAccountID(loaded.OpenID)
	targets := make([]apphost.ObservationTarget, 0, len(loaded.VideoIDs))
	for _, videoID := range loaded.VideoIDs {
		targets = append(targets, apphost.ObservationTarget{
			ConnectionRef: apphost.ConnectionRef{AppID: ID, InstallationID: installationID, ConnectionID: ownVideoConnectionID, AccountID: accountID},
			ContentID:     videoID,
			Label:         "已授权本人作品",
			Fields:        []apphost.ObservationField{apphost.ObservationOwnVideo},
		})
	}
	return targets, nil
}

func (p *SearchApp) ownVideoGrant(ctx context.Context, access apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	if err := ctx.Err(); err != nil {
		return apphost.ObservationGrant{}, err
	}
	loaded, err := p.loadOwnVideo()
	if err != nil || !p.ownVideoAllowed(loaded, access) {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	if !p.nowUTC().Before(loaded.expiresAt) {
		return apphost.ObservationGrant{}, apphost.ErrObservationCredentialExpired
	}
	return apphost.ObservationGrant{Allowed: true, Version: loaded.version}, nil
}

func (p *SearchApp) readOwnVideo(ctx context.Context, request apphost.ObservationRequest) (apphost.ObservationPage, error) {
	if request.Field != apphost.ObservationOwnVideo || request.Cursor != "" || (request.Limit != 0 && request.Limit != 1) {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	loaded, err := p.loadOwnVideo()
	if err != nil || !p.ownVideoAllowed(loaded, request.ObservationAccess) {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	if !p.nowUTC().Before(loaded.expiresAt) {
		return apphost.ObservationPage{}, apphost.ErrObservationCredentialExpired
	}
	if err := p.authorizeOwnVideo(loaded, request.ObservationAccess); err != nil {
		return apphost.ObservationPage{}, err
	}
	video, err := p.fetchOwnVideo(ctx, loaded, request.ContentID)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	// A revocation, expiry, or local policy change while the request was in
	// flight must invalidate the result before the host exposes it.
	if err := p.authorizeOwnVideo(loaded, request.ObservationAccess); err != nil {
		return apphost.ObservationPage{}, err
	}
	return buildOwnVideoPage(video, request.ContentID, p.nowUTC())
}

func (p *SearchApp) authorizeOwnVideo(previous loadedOwnVideoSettings, access apphost.ObservationAccess) error {
	current, err := p.loadOwnVideo()
	if err != nil || current.version != previous.version || !p.ownVideoAllowed(current, access) {
		return apphost.ErrObservationForbidden
	}
	if !p.nowUTC().Before(current.expiresAt) {
		return apphost.ErrObservationCredentialExpired
	}
	return nil
}

func (p *SearchApp) ownVideoAllowed(loaded loadedOwnVideoSettings, access apphost.ObservationAccess) bool {
	if !loaded.Enabled || access.AppID != ID || access.InstallationID != installationID || access.ConnectionID != ownVideoConnectionID || access.AccountID != ownVideoAccountID(loaded.OpenID) || access.Field != apphost.ObservationOwnVideo {
		return false
	}
	for _, videoID := range loaded.VideoIDs {
		if access.ContentID == videoID {
			return true
		}
	}
	return false
}

func (p *SearchApp) loadOwnVideo() (loadedOwnVideoSettings, error) {
	if p == nil || strings.TrimSpace(p.privateDirectory) == "" {
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	root, err := workspacefs.OpenPath(p.privateDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loadedOwnVideoSettings{}, errOwnVideoUnconfigured
		}
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, ownVideoSettingsName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return loadedOwnVideoSettings{}, errOwnVideoUnconfigured
		}
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil || len(raw) > maxSettingsBytes {
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	var decoded ownVideoSettings
	if json.Unmarshal(raw, &decoded) != nil {
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	if !validOwnVideoShape(raw) {
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	expiresAt, err := validateOwnVideoSettings(decoded)
	if err != nil {
		return loadedOwnVideoSettings{}, ErrInvalid
	}
	digest := sha256.Sum256(raw)
	return loadedOwnVideoSettings{ownVideoSettings: decoded, expiresAt: expiresAt, version: "douyin-own-video-" + hex.EncodeToString(digest[:])}, nil
}

func validOwnVideoShape(raw []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != 7 {
		return false
	}
	for _, key := range []string{"schema_version", "enabled", "scope", "access_token", "open_id", "expires_at", "video_ids"} {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	return true
}

func validateOwnVideoSettings(value ownVideoSettings) (time.Time, error) {
	if value.SchemaVersion != 1 || value.Scope != "posting.behavior" || !accessToken.MatchString(value.AccessToken) || !openID.MatchString(value.OpenID) || len(value.VideoIDs) == 0 || len(value.VideoIDs) > 100 {
		return time.Time{}, ErrInvalid
	}
	expiresAt, err := time.Parse(time.RFC3339, value.ExpiresAt)
	if err != nil || expiresAt.IsZero() {
		return time.Time{}, ErrInvalid
	}
	seen := make(map[string]struct{}, len(value.VideoIDs))
	for _, videoID := range value.VideoIDs {
		if !decimalID.MatchString(videoID) {
			return time.Time{}, ErrInvalid
		}
		if _, duplicate := seen[videoID]; duplicate {
			return time.Time{}, ErrInvalid
		}
		seen[videoID] = struct{}{}
	}
	return expiresAt.UTC(), nil
}

type ownVideoResponse struct {
	Data *struct {
		ErrorCode *int           `json:"error_code"`
		List      []ownVideoInfo `json:"list"`
	} `json:"data"`
	Extra *struct {
		ErrorCode *int `json:"error_code"`
	} `json:"extra"`
}

type ownVideoInfo struct {
	VideoID    json.RawMessage `json:"video_id"`
	ItemID     string          `json:"item_id"`
	Status     *int            `json:"video_status"`
	Title      string          `json:"title"`
	CreateTime int64           `json:"create_time"`
}

func (p *SearchApp) fetchOwnVideo(ctx context.Context, loaded loadedOwnVideoSettings, requestedID string) (ownVideoInfo, error) {
	return p.fetchVideoStatus(ctx, loaded, requestedID, false)
}

func (p *SearchApp) fetchVideoStatus(ctx context.Context, loaded loadedOwnVideoSettings, requestedID string, encrypted bool) (ownVideoInfo, error) {
	if p == nil || p.client == nil {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	endpoint, err := url.Parse(ownVideoEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "open.douyin.com" {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	query := endpoint.Query()
	query.Set("open_id", loaded.OpenID)
	endpoint.RawQuery = query.Encode()
	key := "video_ids"
	if encrypted {
		key = "item_ids"
	}
	payload, err := json.Marshal(map[string][]string{key: {requestedID}})
	if err != nil {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(readCtx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Agent-home-station/1.0 douyin-own-video")
	req.Header.Set("access-token", loaded.AccessToken)
	response, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ownVideoInfo{}, ctx.Err()
		}
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusPreconditionFailed {
		return ownVideoInfo{}, apphost.ErrObservationThrottled
	}
	if response.StatusCode != http.StatusOK {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	var decoded ownVideoResponse
	if json.Unmarshal(body, &decoded) != nil {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	// Report an explicit nonzero envelope even when a broken upstream omits
	// its companion envelope. Successful reads, however, require both.
	if decoded.Data != nil && decoded.Data.ErrorCode != nil && *decoded.Data.ErrorCode != 0 {
		return ownVideoInfo{}, ownVideoResponseError(*decoded.Data.ErrorCode)
	}
	if decoded.Extra != nil && decoded.Extra.ErrorCode != nil && *decoded.Extra.ErrorCode != 0 {
		return ownVideoInfo{}, ownVideoResponseError(*decoded.Extra.ErrorCode)
	}
	if decoded.Data == nil || decoded.Data.ErrorCode == nil || decoded.Extra == nil || decoded.Extra.ErrorCode == nil {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	if err := ownVideoResponseError(*decoded.Data.ErrorCode); err != nil {
		return ownVideoInfo{}, err
	}
	if err := ownVideoResponseError(*decoded.Extra.ErrorCode); err != nil {
		return ownVideoInfo{}, err
	}
	if len(decoded.Data.List) == 0 {
		return ownVideoInfo{}, apphost.ErrObservationContentUnavailable
	}
	if len(decoded.Data.List) != 1 {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	video := decoded.Data.List[0]
	observed, _ := canonicalItemID(video.VideoID)
	if encrypted {
		observed = video.ItemID
	}
	if observed != requestedID {
		return ownVideoInfo{}, apphost.ErrObservationUpstreamUnavailable
	}
	return video, nil
}

func ownVideoResponseError(code int) error {
	switch code {
	case 0:
		return nil
	case 28001003, 28001008:
		return apphost.ErrObservationCredentialExpired
	case 28001014, 28001016, 28001018, 28001019:
		return apphost.ErrObservationCapabilityDenied
	case 28003017:
		return apphost.ErrObservationThrottled
	case 2118110:
		return apphost.ErrObservationThrottled
	case 2100004, 28001005, 28001006:
		return apphost.ErrObservationUpstreamUnavailable
	case 2100005:
		return apphost.ErrObservationInvalid
	default:
		return apphost.ErrObservationContentUnavailable
	}
}

func buildOwnVideoPage(video ownVideoInfo, requestedID string, observed time.Time) (apphost.ObservationPage, error) {
	state := "unknown"
	if video.Status == nil {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	switch *video.Status {
	case 2:
		state = "not_public"
	case 4:
		state = "reviewing"
	case 5:
		state = "public"
	}
	page := apphost.ObservationPage{
		Coverage:      "抖音开放平台投稿作品状态 · 仅核验已授权作品",
		Complete:      true,
		OwnVideoProof: &apphost.OwnVideoProof{VideoID: requestedID, StatusCode: *video.Status, State: state, ObservedAt: observed.UTC().Format(time.RFC3339)},
	}
	if state != "public" {
		return page, nil
	}
	if video.CreateTime < 0 || video.CreateTime > 253402300799 {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	if video.CreateTime > 0 {
		page.OwnVideoProof.CreatedAt = time.Unix(video.CreateTime, 0).UTC().Format(time.RFC3339)
	}
	if title := strings.TrimSpace(video.Title); title != "" {
		chunk := apphost.ObservationChunk{Position: 0, Text: title, SourceID: requestedID}
		page.Chunks = []apphost.ObservationChunk{chunk}
	}
	return page, nil
}
