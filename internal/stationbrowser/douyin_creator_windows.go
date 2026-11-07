//go:build windows

package stationbrowser

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/hostfile"
)

const douyinCreatorOrigin = "https://creator.douyin.com"
const douyinCreatorUpload = douyinCreatorOrigin + "/creator-micro/content/upload"
const douyinCreatorManage = douyinCreatorOrigin + "/creator-micro/content/manage"

//go:embed douyin_creator_ui.js
var douyinCreatorUI string

// OpenDouyinCreatorLogin uses the same private profile and ownership lock as
// ordinary login. Its target is fixed by the host, never by an Agent argument.
func OpenDouyinCreatorLogin(ctx context.Context, profile string) (*LoginWindow, error) {
	return openDouyinLogin(ctx, profile, douyinCreatorUpload, true)
}

func ContinueDouyinPublication(ctx context.Context, profile string, in CreatorPublicationRequest, authorize func() error) (CreatorPublicationResult, error) {
	if in.Submitted {
		return QueryDouyinPublication(ctx, profile, in.RemoteID, authorize)
	}
	return runDouyinCreator(ctx, profile, douyinCreatorUpload, in, authorize, false)
}

func QueryDouyinPublication(ctx context.Context, profile, remoteID string, authorize func() error) (CreatorPublicationResult, error) {
	result := CreatorPublicationResult{Stage: "awaiting_receipt", RemoteID: remoteID, Submitted: true}
	if remoteID == "" {
		return result, ErrCreatorPublicationUnknown
	}
	if !douyinVideoID.MatchString(remoteID) {
		return result, ErrCreatorPublicationInput
	}
	return runDouyinCreator(ctx, profile, douyinCreatorManage, CreatorPublicationRequest{Submitted: true, RemoteID: remoteID}, authorize, false)
}

// FindDouyinPublication only opens the saved account's management page. The
// immutable caption/time/privacy are matched against that page's own response;
// this path cannot upload a file or click the publish control.
func FindDouyinPublication(ctx context.Context, profile string, query CreatorPublicationQuery, authorize func() error) (CreatorPublicationResult, error) {
	if !validCreatorPublicationQuery(query) {
		return CreatorPublicationResult{}, ErrCreatorPublicationInput
	}
	return runDouyinCreator(ctx, profile, douyinCreatorManage, CreatorPublicationRequest{Submitted: true, Query: &query}, authorize, false)
}

func runDouyinCreator(ctx context.Context, profile, target string, in CreatorPublicationRequest, authorize func() error, fixture bool) (CreatorPublicationResult, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
	}
	result := CreatorPublicationResult{Submitted: in.Submitted, RemoteID: in.RemoteID}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if authorize == nil || (!fixture && target != douyinCreatorUpload && target != douyinCreatorManage) {
		return result, ErrCreatorPublicationInput
	}
	if !in.Submitted {
		info, err := os.Stat(in.MediaPath)
		ext := strings.ToLower(filepath.Ext(in.MediaPath))
		if err != nil || !info.Mode().IsRegular() || info.Size() < 12 || !filepath.IsAbs(in.MediaPath) ||
			(ext != ".mp4" && ext != ".webm") || in.TaskID == "" || in.ManifestSHA256 == "" || in.BeforeSubmit == nil ||
			(in.Visibility != "public" && in.Visibility != "private" && in.Visibility != "friends") {
			return result, ErrCreatorPublicationInput
		}
		in.MediaBytes = info.Size()
	}
	if err := authorize(); err != nil {
		return result, err
	}
	lock, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return result, err
	}
	release, err := hostfile.TryLock(lock)
	if err != nil {
		return result, ErrDouyinLoginActive
	}
	defer release()
	var session *cdpSession
	if fixture {
		session, err = openCDP(ctx, profile)
	} else {
		// Creator pages load many independent bundles. Connections are counted
		// at both ends of each CONNECT tunnel; the feed's 16-descriptor limit can
		// otherwise starve essential React/uploader dependencies.
		session, err = openRestrictedCDPWithCapacity(ctx, profile, isDouyinCreatorHost, 128)
	}
	if err != nil {
		return result, ErrBrowserUnavailable
	}
	defer closeDouyinHomeSession(session)
	return runDouyinCreatorSession(ctx, session, target, in, authorize)
}

// The creator SPA has different static/auth dependencies from the public
// feed. These exact hosts were referenced by its public HTML on 2026-11-07;
// granting them here does not expand ordinary feed-reading access.
func isDouyinCreatorHost(host string) bool {
	if isDouyinHomeHost(host) {
		return true
	}
	u, err := url.Parse("https://" + host)
	if err != nil || u.User != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")) {
	case "auth.zijieapi.com", "lf-c-flwb.bytetos.com", "lf-fe-creator.douyinstatic.com",
		"lf-security.bytegoofy.com", "lf-ucenter-web.yhgfb-cn-static.com", "lf1-cdn-tos.bytegoofy.com",
		"lf1-cdn-tos.bytescm.com", "lf3-beecdn.bytetos.com", "lf3-cdn-tos.bytescm.com",
		"s3.bytecdn.cn", "s3a.bytecdn.cn", "unpkg.byted-static.com",
		"mssdk.bytedance.com", "abtestvm.bytedance.com", "creator.amemv.com", "sf3-cdn-tos.douyinstatic.com", "p26.douyinpic.com":
		return true
	case "vcs.zijieapi.com", "lf-rc1.yhgfb-cn-static.com", "lf-rc2.yhgfb-cn-static.com", "lf-cdn-tos.bytescm.com":
		return true
	case "vod.bytedanceapi.com", "open.bytedanceapi.com", "imagex.bytedanceapi.com", "security.zijieapi.com",
		"sf1-cdn-tos.douyinstatic.com", "lf3-config.bytetcc.com", "p11.douyinpic.com", "p3.douyinpic.com":
		return true
	}
	return false
}

type creatorPageState struct {
	Invalid, Login, FileAvailable, EditorAvailable, Filled, VisibilityOK, Enabled, FileStillBound, FileLost, Selected, Clicked, Listed, Published, UploadFailed bool
	PreviewCount                                                                                                                                                int
	PinMatches                                                                                                                                                  bool
	PreviewState                                                                                                                                                string
}

func creatorPage(ctx context.Context, session readerSession, id string, world int64, target, action string, in CreatorPublicationRequest) (creatorPageState, error) {
	origin, err := url.Parse(target)
	if err != nil {
		return creatorPageState{}, ErrCreatorPublicationControls
	}
	input, _ := json.Marshal(map[string]any{"origin": origin.Scheme + "://" + origin.Host, "action": action,
		"title": in.Title, "body": in.Body, "visibility": in.Visibility, "remoteID": in.RemoteID, "mediaBytes": in.MediaBytes})
	var out struct {
		Result struct {
			Type  string
			Value creatorPageState
		}
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if session.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": "(" + douyinCreatorUI + ")(" + string(input) + ")", "contextId": world, "returnByValue": true}, &out) != nil ||
		out.Result.Type != "object" || len(out.Exception) != 0 || out.Result.Value.Invalid {
		return creatorPageState{}, ErrCreatorPublicationControls
	}
	if out.Result.Value.Login {
		return out.Result.Value, ErrDouyinWebLoginRequired
	}
	if out.Result.Value.UploadFailed {
		return out.Result.Value, ErrCreatorPublicationUpload
	}
	return out.Result.Value, nil
}

func runDouyinCreatorSession(ctx context.Context, session readerSession, target string, in CreatorPublicationRequest, authorize func() error) (CreatorPublicationResult, error) {
	result := CreatorPublicationResult{Stage: "opening_creator", Submitted: in.Submitted, RemoteID: in.RemoteID}
	progress := func(stage string) error {
		result.Stage = stage
		if in.Progress != nil {
			return in.Progress(ctx, stage)
		}
		return nil
	}
	if err := progress("opening_creator"); err != nil {
		return result, err
	}
	var created struct{ TargetID string }
	var attached struct{ SessionID string }
	if session.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": true}, &created) != nil || created.TargetID == "" ||
		session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached) != nil || attached.SessionID == "" {
		return result, ErrCreatorPublicationControls
	}
	id := attached.SessionID
	for _, method := range []string{"Page.enable", "DOM.enable", "Performance.enable"} {
		if err := session.Call(ctx, id, method, map[string]any{}, nil); err != nil {
			return result, err
		}
	}
	if err := session.Call(ctx, id, "Network.enable", map[string]any{"maxPostDataSize": 1, "maxResourceBufferSize": 512 << 10, "maxTotalBufferSize": 2 << 20}, nil); err != nil {
		return result, err
	}
	// Activate only this invisible browser's target, so Chromium loads its
	// upload preview rather than suspending media as a background tab.
	if err := session.Call(ctx, id, "Page.bringToFront", map[string]any{}, nil); err != nil {
		return result, err
	}
	// Drain network events from navigation onward. Waiting until submit can
	// overflow the private CDP queue on a resource-heavy creator page.
	listener := newCreatorReceiptListener(target, id)
	listener.query, listener.knownID = in.Query, in.RemoteID
	listener.start(ctx, session)
	defer listener.stop()
	if err := session.Call(ctx, id, "Network.setBypassServiceWorker", map[string]any{"bypass": true}, nil); err != nil {
		return result, err
	}
	if err := session.Call(ctx, id, "Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}, nil); err != nil {
		return result, err
	}
	if err := session.Call(ctx, id, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.open=()=>null;"}, nil); err != nil {
		return result, err
	}
	if err := authorize(); err != nil {
		return result, err
	}
	var nav struct {
		FrameID, ErrorText string
		IsDownload         bool
	}
	if err := session.Call(ctx, id, "Page.navigate", map[string]any{"url": target}, &nav); err != nil || nav.ErrorText != "" || nav.IsDownload {
		return result, ErrCreatorPublicationControls
	}
	// Page.navigate may complete before its execution world can be created.
	var world int64
	var err error
	for world == 0 {
		world, err = createPlaybackWorld(ctx, session, id, nav.FrameID)
		if err == nil {
			break
		}
		if err := creatorWait(ctx); err != nil {
			return result, err
		}
	}
	if in.Submitted {
		result.Stage = "awaiting_receipt"
		for {
			if err := authorize(); err != nil {
				return result, err
			}
			if found, loaded := listener.listResult(); loaded {
				if found.RemoteID == "" {
					return result, ErrCreatorPublicationUnknown
				}
				return found, nil
			}
			// Retain the DOM route for layouts exposing a stable work link.
			// Caption-only recovery always needs the account response with ID.
			if in.RemoteID != "" {
				state, err := creatorPage(ctx, session, id, world, target, "query", in)
				if err != nil {
					return result, err
				}
				if state.Listed {
					result.Stage = "accepted"
					result.Published = state.Published
					if state.Published {
						result.Stage = "published"
					}
					return result, nil
				}
			}
			if err := creatorWait(ctx); err != nil {
				return result, errors.Join(ErrCreatorPublicationUnknown, err)
			}
		}
	}
	result.Stage = "uploading"
	if _, err := waitCreatorState(ctx, session, id, world, target, "snapshot", in, authorize, func(s creatorPageState) bool { return s.FileAvailable }); err != nil {
		return result, err
	}
	var doc struct{ Root struct{ NodeID int } }
	var nodes struct{ NodeIDs []int }
	if session.Call(ctx, id, "DOM.getDocument", map[string]any{}, &doc) != nil ||
		session.Call(ctx, id, "DOM.querySelectorAll", map[string]any{"nodeId": doc.Root.NodeID, "selector": "input[type=file]"}, &nodes) != nil {
		return result, ErrCreatorPublicationControls
	}
	// Select the same unique video input as the isolated DOM inspection. Other
	// file inputs (e.g. a cover image) must never receive the video.
	var videoNodes []int
	for _, node := range nodes.NodeIDs {
		var attrs struct{ Attributes []string }
		if err := session.Call(ctx, id, "DOM.getAttributes", map[string]any{"nodeId": node}, &attrs); err != nil {
			return result, err
		}
		accept := ""
		for i := 0; i+1 < len(attrs.Attributes); i += 2 {
			if attrs.Attributes[i] == "accept" {
				accept = strings.ToLower(attrs.Attributes[i+1])
			}
		}
		if strings.Contains(accept, "video") || strings.Contains(accept, "mp4") || strings.Contains(accept, "webm") {
			videoNodes = append(videoNodes, node)
		}
	}
	if len(videoNodes) != 1 {
		return result, ErrCreatorPublicationControls
	}
	if err := progress("selecting_file"); err != nil {
		return result, err
	}
	if err := authorize(); err != nil {
		return result, err
	}
	if bound, err := creatorPage(ctx, session, id, world, target, "bind-input", in); err != nil || !bound.Selected {
		return result, ErrCreatorPublicationUpload
	}
	if err := session.Call(ctx, id, "DOM.setFileInputFiles", map[string]any{"nodeId": videoNodes[0], "files": []string{in.MediaPath}}, nil); err != nil {
		return result, ErrCreatorPublicationUpload
	}
	if pin, err := creatorPage(ctx, session, id, world, target, "pin-file", in); err != nil || !pin.Selected {
		return result, ErrCreatorPublicationUpload
	}
	if err := progress("uploading"); err != nil {
		return result, err
	}
	if _, err := waitCreatorState(ctx, session, id, world, target, "snapshot", in, authorize, func(s creatorPageState) bool { return s.EditorAvailable }); err != nil {
		return result, err
	}
	if err := progress("editing"); err != nil {
		return result, err
	}
	if _, err := creatorPage(ctx, session, id, world, target, "fill", in); err != nil {
		return result, err
	}
	if err := progress("verifying_form"); err != nil {
		return result, err
	}
	if state, err := waitCreatorState(ctx, session, id, world, target, "snapshot", in, authorize, func(s creatorPageState) bool { return s.Filled && s.VisibilityOK && s.Enabled && s.FileStillBound }); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			switch {
			case !state.Filled:
				return result, errors.Join(ErrCreatorPublicationCaption, err)
			case !state.VisibilityOK:
				return result, errors.Join(ErrCreatorPublicationVisibility, err)
			case !state.FileStillBound:
				return result, errors.Join(ErrCreatorPublicationUpload, err)
			}
		}
		return result, err
	}
	if err := authorize(); err != nil {
		return result, err
	}
	var metrics struct {
		Metrics []struct {
			Name  string
			Value float64
		}
	}
	if err := session.Call(ctx, id, "Performance.getMetrics", map[string]any{}, &metrics); err != nil {
		return result, ErrCreatorPublicationControls
	}
	var submitAfter float64
	for _, metric := range metrics.Metrics {
		if metric.Name == "Timestamp" {
			submitAfter = metric.Value
		}
	}
	if submitAfter <= 0 {
		return result, ErrCreatorPublicationControls
	}
	// A successful durable checkpoint transfers ownership to reconciliation,
	// even if cancellation or a broken pipe happens before click is observed.
	if err := in.BeforeSubmit(ctx); err != nil {
		return result, err
	}
	result.Submitted, result.Stage = true, "awaiting_receipt"
	if err := authorize(); err != nil {
		return result, err
	}
	listener.arm(submitAfter)
	if state, err := creatorPage(ctx, session, id, world, target, "submit", in); err != nil || !state.Clicked {
		return result, ErrCreatorPublicationUnknown
	}
	for {
		remoteID := listener.result()
		if remoteID != "" {
			result.RemoteID, result.Stage = remoteID, "accepted"
			return result, nil
		}
		if code, rejected := listener.rejection(); rejected {
			result.Stage, result.RejectionCode = "rejected", code
			return result, fmt.Errorf("%w（状态码 %d）", ErrCreatorPublicationRejected, code)
		}
		if err := creatorWait(ctx); err != nil {
			return result, errors.Join(ErrCreatorPublicationUnknown, err)
		}
	}
}

func waitCreatorState(ctx context.Context, session readerSession, id string, world int64, target, action string, in CreatorPublicationRequest, authorize func() error, ready func(creatorPageState) bool) (creatorPageState, error) {
	for {
		if err := authorize(); err != nil {
			return creatorPageState{}, err
		}
		state, err := creatorPage(ctx, session, id, world, target, action, in)
		if err != nil {
			return state, err
		}
		if action == "snapshot" && state.FileLost {
			return state, ErrCreatorPublicationUpload
		}
		if ready(state) {
			return state, nil
		}
		if err := creatorWait(ctx); err != nil {
			return state, fmt.Errorf("%w (creator_ready: editor=%t filled=%t visibility=%t enabled=%t bound=%t preview=%d match=%t native=%s)", err, state.EditorAvailable, state.Filled, state.VisibilityOK, state.Enabled, state.FileStillBound, state.PreviewCount, state.PinMatches, state.PreviewState)
		}
	}
}

func creatorWait(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Only a newly observed POST/200 response in this exact owned creator session
// can provide the accepted work ID. No direct or reverse-engineered API is
// called by the host. Response bodies and authentication never leave it.
type creatorReceiptListener struct {
	origin, session string
	mu              sync.Mutex
	remoteID        string
	submitAfter     float64
	rejected        bool
	rejectionCode   int
	cancel          context.CancelFunc
	done            chan struct{}
	query           *CreatorPublicationQuery
	knownID         string
	listLoaded      bool
	listReceipt     CreatorPublicationResult
}

func newCreatorReceiptListener(target, session string) *creatorReceiptListener {
	u, _ := url.Parse(target)
	return &creatorReceiptListener{origin: u.Scheme + "://" + u.Host, session: session, done: make(chan struct{})}
}
func (l *creatorReceiptListener) result() string { l.mu.Lock(); defer l.mu.Unlock(); return l.remoteID }
func (l *creatorReceiptListener) listResult() (CreatorPublicationResult, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.listReceipt, l.listLoaded
}
func (l *creatorReceiptListener) manageEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Scheme+"://"+u.Host == l.origin && u.Path == "/janus/douyin/creator/pc/work_list"
}
func (l *creatorReceiptListener) rejection() (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rejectionCode, l.rejected
}
func (l *creatorReceiptListener) arm(timestamp float64) {
	l.mu.Lock()
	l.submitAfter = timestamp
	l.mu.Unlock()
}
func (l *creatorReceiptListener) afterSubmission(timestamp float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.submitAfter > 0 && timestamp > l.submitAfter
}
func (l *creatorReceiptListener) stop() { l.cancel(); <-l.done }
func (l *creatorReceiptListener) endpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Scheme+"://"+u.Host == l.origin &&
		(u.Path == "/web/api/media/aweme/create/" || u.Path == "/web/api/media/aweme/create_v2/" || u.Path == "/aweme/v1/web/aweme/post/")
}
func (l *creatorReceiptListener) start(parent context.Context, session readerSession) {
	ctx, cancel := context.WithCancel(parent)
	l.cancel = cancel
	go func() {
		defer close(l.done)
		requests, accepted := map[string]bool{}, map[string]bool{}
		manage := map[string]bool{}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-session.Events():
				if !ok {
					return
				}
				if event.SessionID != l.session {
					continue
				}
				switch event.Method {
				case "Network.requestWillBeSent":
					var p struct {
						RequestID string
						Timestamp float64
						Request   struct{ URL, Method string }
					}
					if json.Unmarshal(event.Params, &p) == nil {
						delete(requests, p.RequestID)
						delete(accepted, p.RequestID)
						delete(manage, p.RequestID)
						if (l.query != nil || l.knownID != "") && l.manageEndpoint(p.Request.URL) && (p.Request.Method == "GET" || p.Request.Method == "POST") {
							requests[p.RequestID] = true
							manage[p.RequestID] = true
						}
						if l.afterSubmission(p.Timestamp) && p.Request.Method == "POST" && l.endpoint(p.Request.URL) {
							requests[p.RequestID] = true
						}
					}
				case "Network.responseReceived":
					var p struct {
						RequestID string
						Response  struct {
							URL               string
							Status            int
							FromServiceWorker bool
						}
					}
					if json.Unmarshal(event.Params, &p) == nil && requests[p.RequestID] && p.Response.Status == 200 && !p.Response.FromServiceWorker && (l.endpoint(p.Response.URL) || (manage[p.RequestID] && l.manageEndpoint(p.Response.URL))) {
						accepted[p.RequestID] = true
					}
				case "Network.loadingFinished":
					var p struct {
						RequestID         string
						EncodedDataLength float64
					}
					if json.Unmarshal(event.Params, &p) != nil || !accepted[p.RequestID] {
						continue
					}
					delete(accepted, p.RequestID)
					delete(requests, p.RequestID)
					isManage := manage[p.RequestID]
					delete(manage, p.RequestID)
					if p.EncodedDataLength > 256<<10 {
						continue
					}
					var out struct {
						Body          string
						Base64Encoded bool
					}
					if session.Call(ctx, l.session, "Network.getResponseBody", map[string]any{"requestId": p.RequestID}, &out) != nil || len(out.Body) > 512<<10 {
						continue
					}
					raw := []byte(out.Body)
					if out.Base64Encoded {
						var err error
						raw, err = base64.StdEncoding.DecodeString(out.Body)
						if err != nil {
							continue
						}
					}
					if isManage {
						found := creatorManageReceipt(raw, l.query, l.knownID)
						l.mu.Lock()
						l.listReceipt, l.listLoaded = found, true
						l.mu.Unlock()
						continue
					}
					var status struct {
						Code *int `json:"status_code"`
					}
					if len(raw) <= 256<<10 && json.Unmarshal(raw, &status) == nil && status.Code != nil && *status.Code != 0 {
						l.mu.Lock()
						l.rejected, l.rejectionCode = true, *status.Code
						l.mu.Unlock()
						return
					}
					if id := creatorReceiptID(raw); id != "" {
						l.mu.Lock()
						l.remoteID = id
						l.mu.Unlock()
						return
					}
				}
			}
		}
	}()
}

func creatorReceiptID(raw []byte) string {
	if len(raw) > 256<<10 {
		return ""
	}
	var response struct {
		StatusCode *int            `json:"status_code"`
		AwemeID    json.RawMessage `json:"aweme_id"`
		ItemID     json.RawMessage `json:"item_id"`
		Aweme      struct {
			AwemeID json.RawMessage `json:"aweme_id"`
		} `json:"aweme"`
		Data struct {
			AwemeID json.RawMessage `json:"aweme_id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || response.StatusCode == nil || *response.StatusCode != 0 {
		return ""
	}
	ids := map[string]bool{}
	for _, value := range []json.RawMessage{response.AwemeID, response.ItemID, response.Aweme.AwemeID, response.Data.AwemeID} {
		if len(value) == 0 {
			continue
		}
		id := string(value)
		if value[0] == '"' {
			if json.Unmarshal(value, &id) != nil {
				return ""
			}
		}
		if !douyinVideoID.MatchString(id) {
			return ""
		}
		ids[id] = true
	}
	if len(ids) != 1 {
		return ""
	}
	for id := range ids {
		return id
	}
	return ""
}
