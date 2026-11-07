package station

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/workspacefs"
)

const localDouyinWebHomeURL = "https://www.douyin.com/"
const localDouyinWebSnapshotMax = 8 << 20 // private JSON including metadata, bounded by the CDP transport

type localDouyinWebObservation struct {
	home         *localHomeService
	installation string
	mu           sync.Mutex
}

type localDouyinWebAccess struct {
	record           localDouyinWebRecord
	connection       apphost.Connection
	profile, version string
}

// state is read on every grant, including cached page use. It never obtains a
// platform account identity from an owner confirmation or exposes a profile.
func (p *localDouyinWebObservation) state(ctx context.Context) (localDouyinWebAccess, error) {
	if err := ctx.Err(); err != nil {
		return localDouyinWebAccess{}, err
	}
	identity, err := p.home.identity.Read()
	if err != nil || identity.LocalOwner == nil {
		return localDouyinWebAccess{}, apphost.ErrObservationForbidden
	}
	m := p.home.douyinWeb
	if m == nil {
		return localDouyinWebAccess{}, apphost.ErrObservationBrowserUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return localDouyinWebAccess{}, err
	}
	dir := filepath.Join(p.home.dataDir, "apps", "douyin", "web-login", identity.FishID)
	root, err := workspacefs.OpenPath(dir)
	if errors.Is(err, os.ErrNotExist) {
		return p.access(identity.FishID, identity.LocalOwner.ID, localDouyinWebRecord{Version: 1, FishID: identity.FishID}, localDouyinWebView{State: "unconfigured"}), nil
	}
	if err != nil {
		return localDouyinWebAccess{}, apphost.ErrObservationWebSessionInvalid
	}
	defer root.Close()
	record, err := readLocalDouyinWebRecord(root, identity.FishID)
	if err != nil {
		return localDouyinWebAccess{}, apphost.ErrObservationWebSessionInvalid
	}
	view := m.view(record)
	if record.ReadEnabled && view.State != "saved" {
		return localDouyinWebAccess{}, apphost.ErrObservationWebSessionInvalid
	}
	return p.access(identity.FishID, identity.LocalOwner.ID, record, view), nil
}

func (p *localDouyinWebObservation) access(fish, owner string, r localDouyinWebRecord, v localDouyinWebView) localDouyinWebAccess {
	// This identifies a host-owned profile scope, never a verified Douyin user.
	accountKey := fish + "\x00" + r.Profile
	if r.ProfileEpoch > 0 {
		accountKey += fmt.Sprintf("\x00%d", r.ProfileEpoch)
	}
	account := sha256.Sum256([]byte(accountKey))
	state := "unconfigured"
	if r.Profile != "" {
		state = "disabled"
	}
	if v.ReadEnabled && v.Available {
		state = "ready"
	}
	raw, _ := json.Marshal(r)
	version := sha256.Sum256(append([]byte("douyin-web-text-v2\x00"+fish+"\x00"+owner+"\x00"), raw...))
	return localDouyinWebAccess{record: r, connection: apphost.Connection{ID: douyinapp.WebReadConnectionID,
		AccountID: "web-profile-" + hex.EncodeToString(account[:20]), Label: "抖音网页内容", Configuration: state},
		profile: filepath.Join(p.home.dataDir, "apps", "douyin", "web-login", fish, r.Profile), version: hex.EncodeToString(version[:])}
}

func (p *localDouyinWebObservation) Connection(ctx context.Context) (apphost.Connection, bool, error) {
	if err := ctx.Err(); err != nil {
		return apphost.Connection{}, false, err
	}
	a, err := p.state(ctx)
	if errors.Is(err, apphost.ErrObservationForbidden) {
		return apphost.Connection{}, false, nil
	}
	if err != nil {
		return apphost.Connection{ID: douyinapp.WebReadConnectionID, Label: "抖音网页内容", Configuration: "invalid"}, true, nil
	}
	return a.connection, true, nil
}

func (p *localDouyinWebObservation) ref(a localDouyinWebAccess) apphost.ConnectionRef {
	return apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: a.connection.ID, AccountID: a.connection.AccountID}
}

func (p *localDouyinWebObservation) Targets(ctx context.Context) ([]apphost.ObservationTarget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, err := p.state(ctx)
	if err != nil || a.connection.Configuration != "ready" {
		return []apphost.ObservationTarget{}, nil
	}
	return []apphost.ObservationTarget{{ConnectionRef: p.ref(a), ContentID: douyinapp.WebReadContentID,
		Fields: []apphost.ObservationField{apphost.ObservationPageText}}}, nil
}

func (p *localDouyinWebObservation) Grant(ctx context.Context, in apphost.ObservationAccess) (apphost.ObservationGrant, error) {
	a, err := p.state(ctx)
	if err != nil {
		return apphost.ObservationGrant{}, err
	}
	if a.connection.Configuration != "ready" || in.ConnectionRef != p.ref(a) || !localDouyinWebFieldAllowed(a.record, in.ContentID, in.Field) {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	if _, ok := p.resolveTarget(a, in.ContentID); !ok {
		return apphost.ObservationGrant{Allowed: false}, nil
	}
	version := p.pageObservationVersion(ctx, a, in.Field)
	if in.Field == apphost.ObservationAudio {
		audio, err := p.audioBinding(ctx)
		if err != nil {
			return apphost.ObservationGrant{}, err
		}
		asrVersion, err := audio.ready(ctx)
		if err != nil || strings.TrimSpace(asrVersion) == "" {
			return apphost.ObservationGrant{}, apphost.ErrObservationAudioUnavailable
		}
		version += "/" + asrVersion
	} else if in.Field == apphost.ObservationVisual {
		visualVersion, err := p.visualBinding().ready(ctx)
		if err != nil {
			return apphost.ObservationGrant{}, err
		}
		version += "/" + visualVersion
	}
	return apphost.ObservationGrant{Allowed: true, Version: version}, nil
}

type localDouyinWebSnapshot struct {
	Version    int                            `json:"schema_version"`
	Grant      string                         `json:"grant"`
	ID         string                         `json:"id"`
	Title      string                         `json:"title"`
	Text       string                         `json:"text"`
	CapturedAt string                         `json:"captured_at"`
	ContentID  string                         `json:"content_id,omitempty"`
	SourceURL  string                         `json:"source_url,omitempty"`
	Links      []stationbrowser.DouyinWebLink `json:"links,omitempty"`
}
type localDouyinWebCursor struct {
	ID     string `json:"snapshot"`
	Offset int    `json:"offset"`
}

func (p *localDouyinWebObservation) Read(ctx context.Context, in apphost.ObservationRequest) (apphost.ObservationPage, error) {
	if in.Limit < 0 {
		return apphost.ObservationPage{}, apphost.ErrObservationInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.state(ctx)
	if err != nil {
		return apphost.ObservationPage{}, err
	}
	if a.connection.Configuration != "ready" || in.ConnectionRef != p.ref(a) || !localDouyinWebFieldAllowed(a.record, in.ContentID, in.Field) {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	target, ok := p.resolveTarget(a, in.ContentID)
	if !ok {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	expectedVersion := p.pageObservationVersion(ctx, a, in.Field)
	if in.Field == apphost.ObservationAudio || in.Field == apphost.ObservationVisual {
		grant, err := p.Grant(ctx, in.ObservationAccess)
		if err != nil {
			return apphost.ObservationPage{}, err
		}
		if !grant.Allowed {
			return apphost.ObservationPage{}, apphost.ErrObservationForbidden
		}
		expectedVersion = grant.Version
	}
	authorize := func() error {
		current, err := p.Grant(ctx, in.ObservationAccess)
		if err != nil {
			return err
		}
		if !current.Allowed || current.Version != expectedVersion {
			return apphost.ErrObservationForbidden
		}
		return nil
	}
	if in.Field == apphost.ObservationPlayback {
		return p.readPlayback(ctx, a, in, target, authorize)
	}
	if in.Field == apphost.ObservationAudio {
		return p.readAudio(ctx, a, in, target, authorize)
	}
	if in.Field == apphost.ObservationVisual {
		return p.readVisual(ctx, a, in, target, authorize)
	}
	if in.Field == apphost.ObservationComments {
		return p.readComments(ctx, a, in, target, authorize)
	}
	dir := p.snapshotDirectory(a)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
	}
	defer root.Close()
	unlock, err := hostfile.TryLock(filepath.Join(dir, "snapshot.lock"))
	if err != nil {
		return apphost.ObservationPage{}, apphost.ErrObservationWebSessionBusy
	}
	defer unlock()
	var snapshot localDouyinWebSnapshot
	var cursor localDouyinWebCursor
	if in.Cursor == "" {
		m := p.home.douyinWeb
		m.mu.Lock()
		readPage, readVideo, serviceCtx := m.readPage, m.readVideo, m.ctx
		m.mu.Unlock()
		if target.videoID == "" && readPage == nil || target.videoID != "" && readVideo == nil {
			return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
		}
		readCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		stop := context.AfterFunc(serviceCtx, cancel)
		defer stop()
		defer cancel()
		var page stationbrowser.DouyinWebPage
		var readErr error
		if target.videoID == "" {
			page, readErr = readPage(readCtx, a.profile, authorize)
		} else {
			page, readErr = readVideo(readCtx, a.profile, target.videoID, authorize)
		}
		if readErr != nil {
			return apphost.ObservationPage{}, localDouyinWebReadError(readErr)
		}
		if err = authorize(); err != nil {
			return apphost.ObservationPage{}, err
		}
		if strings.TrimSpace(page.Text) == "" {
			return apphost.ObservationPage{}, apphost.ErrObservationContentUnavailable
		}
		id, err := localDouyinWebID()
		if err != nil {
			return apphost.ObservationPage{}, errors.New("无法登记网页读取编号")
		}
		snapshot = localDouyinWebSnapshot{Version: 1, Grant: a.version, ID: id, Title: page.Title, Text: page.Text, CapturedAt: time.Now().UTC().Format(time.RFC3339), ContentID: in.ContentID, SourceURL: target.url}
		if target.videoID == "" && localDouyinWebScope(a.record) == "home_and_videos" {
			snapshot.Links = page.Links
		}
		raw, err := json.Marshal(snapshot)
		if err != nil || len(raw) > localDouyinWebSnapshotMax {
			return apphost.ObservationPage{}, apphost.ErrObservationPageTextSize
		}
		if hostfile.WritePrivate(filepath.Join(dir, snapshot.ID+".json"), raw) != nil {
			return apphost.ObservationPage{}, apphost.ErrObservationWebStorageUnavailable
		}
		if err := saveLocalDouyinWebLinkProof(dir, snapshot); err != nil {
			return apphost.ObservationPage{}, err
		}
		cursor.ID = snapshot.ID
	} else {
		raw, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil || len(raw) > 512 || json.Unmarshal(raw, &cursor) != nil || cursor.Offset <= 0 || !validLocalDouyinWebID(cursor.ID) {
			return apphost.ObservationPage{}, apphost.ErrObservationInvalid
		}
		snapshot, err = readLocalDouyinWebSnapshot(root, cursor, a.version, in.ContentID)
		if err != nil {
			return apphost.ObservationPage{}, err
		}
	}
	if err = authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	page, err := localDouyinWebPage(snapshot, cursor.Offset, in.Limit)
	if err == nil {
		fields := p.mediaFollowUps(ctx, a.record)
		if target.videoID != "" && len(page.Chunks) > 0 && len(fields) > 0 {
			first := &page.Chunks[0]
			first.FollowUpContentID = in.ContentID
			first.FollowUpFields = fields
			first.FollowUpField = fields[0]
		}
		if target.videoID == "" {
			for i := range page.Chunks {
				chunk := &page.Chunks[i]
				if chunk.FollowUpContentID != "" {
					chunk.FollowUpFields = append([]apphost.ObservationField{apphost.ObservationPageText}, fields...)
				}
			}
		}
	}
	if err == nil {
		err = authorize()
	}
	return page, err
}

func (p *localDouyinWebObservation) pageObservationVersion(ctx context.Context, a localDouyinWebAccess, field apphost.ObservationField) string {
	// The checkpoint includes follow-up fields. A cached text page may
	// advertise vision only while its runtime remains callable.
	if field == apphost.ObservationPageText && a.record.VisualEnabled {
		if _, err := p.visualBinding().ready(ctx); err == nil {
			return a.version + "/visual-ready"
		}
	}
	return a.version
}

func (p *localDouyinWebObservation) mediaFollowUps(ctx context.Context, record localDouyinWebRecord) []apphost.ObservationField {
	var fields []apphost.ObservationField
	if record.AudioEnabled {
		if audio, err := p.audioBinding(ctx); err == nil {
			if version, err := audio.ready(ctx); err == nil && strings.TrimSpace(version) != "" {
				fields = append(fields, apphost.ObservationAudio)
			}
		}
	}
	if record.PlaybackEnabled {
		fields = append(fields, apphost.ObservationPlayback)
	}
	if record.VisualEnabled {
		if _, err := p.visualBinding().ready(ctx); err == nil {
			fields = append(fields, apphost.ObservationVisual)
		}
	}
	if record.CommentsEnabled {
		fields = append(fields, apphost.ObservationComments)
	}
	return fields
}

func fmtLocalDouyinWebCoverage(start, end, total int, captured string, links bool) string {
	label := "网页可见文字"
	if links {
		label = "网页可见文字与视频链接清单"
	}
	return fmt.Sprintf("%s第 %d–%d 字，共 %d 字；读取时间 %s", label, start+1, end, total, captured)
}

func localDouyinWebReadError(err error) error {
	switch {
	case errors.Is(err, apphost.ErrObservationForbidden):
		return apphost.ErrObservationForbidden
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return apphost.ErrObservationWebReadTimeout
	case errors.Is(err, stationbrowser.ErrDouyinLoginActive):
		return apphost.ErrObservationWebSessionBusy
	case errors.Is(err, stationbrowser.ErrDouyinWebLoginRequired):
		return apphost.ErrObservationWebSessionRequired
	case errors.Is(err, stationbrowser.ErrDouyinWebTextSize):
		return apphost.ErrObservationPageTextSize
	case errors.Is(err, stationbrowser.ErrDouyinCommentsTooLarge):
		return apphost.ErrObservationPageTextSize
	case errors.Is(err, stationbrowser.ErrDouyinCommentsUnavailable):
		return apphost.ErrObservationContentUnavailable
	case errors.Is(err, stationbrowser.ErrPageUnavailable):
		return apphost.ErrObservationContentUnavailable
	default:
		return apphost.ErrObservationBrowserUnavailable
	}
}
