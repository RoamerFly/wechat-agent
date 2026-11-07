package stationbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wechat-bot/internal/workspacefs"
)

const (
	douyinPlayerHost       = "open.douyin.com"
	douyinPlayerPath       = "/player/video"
	douyinEmbedPath        = "/api/douyin/v1/open/auth/embed_video/"
	douyinDetailHost       = "www.douyin.com"
	douyinDetailPath       = "/aweme/v1/web/aweme/detail/"
	douyinStaticHost       = "lf-developer-cdn.open-douyin.com"
	douyinSecurityHost     = "lf-c-flwb.bytetos.com"
	douyinMediaHost        = "v26-web.douyinvod.com"
	douyinStaticPath       = "/obj/open-platform-scm/open_douyin_sdk_demo/static/"
	douyinSecurityPath     = "/obj/rc-client-security/c-webmssdk/"
	douyinMediaPath        = "/"
	remotePlaybackTimeout  = 12 * time.Second
	remotePlaybackPollTick = 150 * time.Millisecond
)

// SampleDouyinPlayer measures a short interval played by Douyin's official
// embedded player. It is intentionally not a general URL browser: the target,
// document, metadata request and media host are all fixed by this package.
//
// The Edge instance has a new profile for this run, is routed through the
// restricted CONNECT-only egress worker, and never reads the user's browser
// profile. Signed media URLs stay inside Edge and are never returned.
func (r *Reader) SampleDouyinPlayer(ctx context.Context, target string, sample time.Duration, authorize func() error) (PlaybackSample, error) {
	if err := ctx.Err(); err != nil {
		return PlaybackSample{}, err
	}
	videoID, ok := canonicalDouyinPlayer(target)
	if r == nil || authorize == nil || !ok || sample < minPlaybackSample || sample > maxPlaybackSample {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := authorize(); err != nil {
		return PlaybackSample{}, err
	}

	profile, cleanup, err := newRemotePlaybackProfile(r.profile)
	if err != nil {
		return PlaybackSample{}, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
	}
	defer cleanup()
	playCtx, cancel := context.WithTimeout(ctx, sample+remotePlaybackTimeout+8*time.Second)
	defer cancel()

	session, err := openRestrictedCDP(playCtx, profile, isDouyinPlaybackHost)
	if err != nil {
		return PlaybackSample{}, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
	}
	defer session.Close()
	return sampleDouyinInSession(playCtx, session, target, videoID, sample, authorize)
}

func newRemotePlaybackProfile(root string) (string, func(), error) {
	if strings.TrimSpace(root) == "" {
		return "", nil, errors.New("private profile root is empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", nil, err
	}
	root = filepath.Clean(root)
	if root == filepath.VolumeName(root)+string(filepath.Separator) || root == "." {
		return "", nil, errors.New("invalid private profile root")
	}
	guard, err := workspacefs.EnsurePath(root)
	if err != nil {
		return "", nil, err
	}
	run, err := os.MkdirTemp(root, "douyin-playback-")
	if err != nil {
		_ = guard.Close()
		return "", nil, err
	}
	child := filepath.Base(run)
	info, err := guard.Lstat(child)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = guard.RemoveAll(child)
		_ = guard.Close()
		return "", nil, errors.New("private playback profile changed while creating")
	}
	cleanup := func() {
		_ = guard.RemoveAll(child)
		_ = guard.Close()
	}
	return run, cleanup, nil
}

func sampleDouyinInSession(ctx context.Context, session readerSession, target, videoID string, sample time.Duration, authorize func() error) (PlaybackSample, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := session.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": false}, &created); err != nil || created.TargetID == "" {
		return PlaybackSample{}, playbackCallError(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return PlaybackSample{}, playbackCallError(err)
	}
	for _, method := range []string{"Page.enable", "Network.enable", "Runtime.enable"} {
		if err := session.Call(ctx, attached.SessionID, method, map[string]any{}, nil); err != nil {
			return PlaybackSample{}, playbackCallError(err)
		}
	}
	if err := session.Call(ctx, attached.SessionID, "Page.bringToFront", map[string]any{}, nil); err != nil {
		return PlaybackSample{}, playbackCallError(err)
	}
	for _, call := range []struct {
		method string
		params any
	}{
		{"Network.setBypassServiceWorker", map[string]any{"bypass": true}},
		{"Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": remotePlaybackPrelude}},
		{"Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*", "requestStage": "Request"}}, "handleAuthRequests": true}},
	} {
		if err := session.Call(ctx, attached.SessionID, call.method, call.params, nil); err != nil {
			return PlaybackSample{}, playbackCallError(err)
		}
	}

	gate := newDouyinPlaybackGate(target, videoID, attached.SessionID, authorize)
	gate.start(ctx, session)
	defer gate.stop()
	if err := authorize(); err != nil {
		return PlaybackSample{}, err
	}
	var navigation struct {
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if err := session.Call(ctx, attached.SessionID, "Page.navigate", map[string]any{"url": target}, &navigation); err != nil {
		return PlaybackSample{}, playbackCallError(err)
	}
	if navigation.ErrorText != "" || navigation.IsDownload {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}
	if err := gate.waitDocument(ctx); err != nil {
		if authErr := gate.authorizationError(); authErr != nil {
			return PlaybackSample{}, authErr
		}
		return PlaybackSample{}, remotePlaybackGateError(err)
	}
	world, err := createPlaybackWorld(ctx, session, attached.SessionID, gate.frameID())
	if err != nil {
		return PlaybackSample{}, err
	}

	readyDeadline := time.NewTimer(remotePlaybackTimeout)
	defer readyDeadline.Stop()
	var state observedPlayback
	for {
		if err := authorize(); err != nil {
			return PlaybackSample{}, err
		}
		state, err = remotePlaybackState(ctx, session, attached.SessionID, world)
		if err != nil {
			return PlaybackSample{}, err
		}
		if state.Present && state.Ready {
			break
		}
		select {
		case <-ctx.Done():
			return PlaybackSample{}, ctx.Err()
		case <-readyDeadline.C:
			return PlaybackSample{}, ErrPlaybackUnavailable
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := authorize(); err != nil {
		return PlaybackSample{}, err
	}
	if err := remoteStartPlayback(ctx, session, attached.SessionID, world); err != nil {
		return PlaybackSample{}, err
	}

	started := time.Now()
	for time.Since(started) < sample {
		select {
		case <-ctx.Done():
			return PlaybackSample{}, ctx.Err()
		case <-time.After(remotePlaybackPollTick):
		}
		if err := authorize(); err != nil {
			return PlaybackSample{}, err
		}
		state, err = remotePlaybackState(ctx, session, attached.SessionID, world)
		if err != nil {
			return PlaybackSample{}, err
		}
		if state.Ended {
			break
		}
	}
	if err := remotePausePlayback(ctx, session, attached.SessionID, world); err != nil {
		return PlaybackSample{}, err
	}
	state, err = remotePlaybackState(ctx, session, attached.SessionID, world)
	if err != nil {
		return PlaybackSample{}, err
	}
	if err := authorize(); err != nil {
		return PlaybackSample{}, err
	}
	if err := gate.validate(); err != nil {
		if authErr := gate.authorizationError(); authErr != nil {
			return PlaybackSample{}, authErr
		}
		return PlaybackSample{}, remotePlaybackGateError(err)
	}
	return state.toSample(), nil
}

// canonicalDouyinPlayer accepts only the URL produced by the official iframe
// API after our adapter reconstructs it. Keeping the raw query canonical makes
// redirects, alternate players and caller-injected options fail closed.
func canonicalDouyinPlayer(target string) (string, bool) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host != douyinPlayerHost || u.User != nil || u.Port() != "" || u.Path != douyinPlayerPath || u.Fragment != "" {
		return "", false
	}
	if u.RawQuery == "" {
		return "", false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values) != 2 || len(values["autoplay"]) != 1 || len(values["vid"]) != 1 || values.Get("autoplay") != "0" || u.RawQuery != "autoplay=0&vid="+url.QueryEscape(values.Get("vid")) {
		return "", false
	}
	for _, r := range values.Get("vid") {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return values.Get("vid"), values.Get("vid") != ""
}

func isDouyinPlaybackHost(host string) bool {
	return host == douyinPlayerHost || host == douyinDetailHost || host == douyinStaticHost || host == douyinSecurityHost || host == douyinMediaHost
}

const remotePlaybackPrelude = `(() => {
  try { Object.defineProperty(window, 'open', {value: () => null, configurable: false, writable: false}); } catch (_) {}
  try { navigator.sendBeacon = () => false; } catch (_) {}
})()`

type douyinPlaybackGate struct {
	target, videoID, sessionID string
	authorize                  func() error
	documentDone               chan struct{}
	stopCh                     chan struct{}
	readyOnce                  sync.Once
	mu                         sync.Mutex
	err                        error
	authorization              error
	frame                      string
	status                     int
	loaded                     bool
	documentAllowed            bool
	embedVerified              bool
	detailVerified             bool
	mediaSeen                  bool
}

func newDouyinPlaybackGate(target, videoID, sessionID string, authorize func() error) *douyinPlaybackGate {
	return &douyinPlaybackGate{target: target, videoID: videoID, sessionID: sessionID, authorize: authorize, documentDone: make(chan struct{}), stopCh: make(chan struct{})}
}

func (g *douyinPlaybackGate) start(ctx context.Context, session readerSession) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				g.finish(ctx.Err())
				return
			case <-g.stopCh:
				return
			case event, ok := <-session.Events():
				if !ok {
					g.finish(ErrPlaybackUnavailable)
					return
				}
				if event.SessionID != g.sessionID {
					continue
				}
				switch event.Method {
				case "Fetch.requestPaused":
					if err := g.handlePaused(ctx, session, event.Params); err != nil {
						g.finish(err)
						return
					}
				case "Fetch.authRequired":
					if err := g.cancelAuth(ctx, session, event.Params); err != nil {
						g.finish(err)
						return
					}
				case "Network.responseReceived":
					g.recordResponse(event.Params)
				case "Page.frameNavigated":
					g.recordFrame(event.Params)
				case "Page.windowOpen", "Page.frameAttached", "Page.downloadWillBegin":
					g.finish(ErrPlaybackUnavailable)
					return
				case "Page.loadEventFired", "Page.lifecycleEvent":
					g.markLoaded(event.Method, event.Params)
				}
			}
		}
	}()
}

func (g *douyinPlaybackGate) stop() { close(g.stopCh) }
func (g *douyinPlaybackGate) finish(err error) {
	g.mu.Lock()
	if err != nil && g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
	g.readyOnce.Do(func() { close(g.documentDone) })
}

func (g *douyinPlaybackGate) handlePaused(ctx context.Context, session readerSession, raw json.RawMessage) error {
	var paused struct {
		RequestID    string `json:"requestId"`
		FrameID      string `json:"frameId"`
		ResourceType string `json:"resourceType"`
		Request      struct {
			URL      string `json:"url"`
			Method   string `json:"method"`
			PostData string `json:"postData"`
		} `json:"request"`
	}
	if json.Unmarshal(raw, &paused) != nil || paused.RequestID == "" {
		return ErrPlaybackUnavailable
	}
	if err := g.authorize(); err != nil {
		g.mu.Lock()
		if g.authorization == nil {
			g.authorization = err
		}
		g.mu.Unlock()
		return err
	}
	allowed, markEmbed, markDetail, _ := g.allowRequest(paused.ResourceType, paused.Request.URL, paused.Request.Method, paused.Request.PostData, paused.FrameID)
	method, params := "Fetch.failRequest", map[string]any{"requestId": paused.RequestID, "errorReason": "BlockedByClient"}
	if allowed {
		method, params = "Fetch.continueRequest", map[string]any{"requestId": paused.RequestID}
		g.mu.Lock()
		g.embedVerified = g.embedVerified || markEmbed
		g.detailVerified = g.detailVerified || markDetail
		g.mu.Unlock()
	}
	return session.Call(ctx, g.sessionID, method, params, nil)
}

func (g *douyinPlaybackGate) allowRequest(resourceType, rawURL, method, postData, frameID string) (allowed, embed, detail, media bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Port() != "" {
		return false, false, false, false
	}
	if resourceType == "Document" {
		g.mu.Lock()
		oneDocument := !g.documentAllowed && frameID != "" && method == "GET" && rawURL == g.target
		if oneDocument {
			g.documentAllowed = true
			g.frame = frameID
		}
		g.mu.Unlock()
		return oneDocument, false, false, false
	}
	g.mu.Lock()
	inTopFrame := frameID != "" && frameID == g.frame
	g.mu.Unlock()
	if !inTopFrame {
		return false, false, false, false
	}
	switch u.Host {
	case douyinStaticHost:
		return method == "GET" && (strings.HasPrefix(u.Path, douyinStaticPath+"js/") || strings.HasPrefix(u.Path, douyinStaticPath+"css/")) && u.RawQuery == "", false, false, false
	case douyinSecurityHost:
		return method == "GET" && strings.HasPrefix(u.Path, douyinSecurityPath) && strings.HasSuffix(u.Path, "/webmssdk.js"), false, false, false
	case douyinPlayerHost:
		allowed = method == "POST" && u.Path == douyinEmbedPath && u.RawQuery == "" && exactEmbedItemID(postData, g.videoID)
		return allowed, allowed, false, false
	case douyinDetailHost:
		query, parseErr := url.ParseQuery(u.RawQuery)
		allowed = method == "GET" && parseErr == nil && u.Path == douyinDetailPath && exactDetailQuery(query, g.videoID)
		return allowed, false, allowed, false
	case douyinMediaHost:
		allowed = method == "GET" && isExpectedDouyinMediaPath(u.Path) && u.RawQuery != ""
		return allowed, false, false, allowed
	default:
		return false, false, false, false
	}
}

func exactEmbedItemID(postData, videoID string) bool {
	if len(postData) == 0 || len(postData) > 256 {
		return false
	}
	dec := json.NewDecoder(bytes.NewBufferString(postData))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') || !dec.More() {
		return false
	}
	name, err := dec.Token()
	if err != nil || name != "item_id" {
		return false
	}
	var got string
	if dec.Decode(&got) != nil || got != videoID || dec.More() {
		return false
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') {
		return false
	}
	var extra any
	return dec.Decode(&extra) == io.EOF
}

func isExpectedDouyinMediaPath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) >= 7 && parts[0] != "" && parts[1] != "" && parts[2] == "video" && parts[3] == "tos" && parts[4] == "cn" && strings.HasPrefix(parts[5], "tos-cn-ve-") && parts[6] != ""
}

func exactDetailQuery(q url.Values, videoID string) bool {
	if len(q) != 5 || q.Get("aid") == "" || q.Get("aweme_id") != videoID || q.Get("X-Bogus") == "" || q.Get("_signature") == "" {
		return false
	}
	for _, key := range []string{"aid", "aweme_id", "msToken", "X-Bogus", "_signature"} {
		if len(q[key]) != 1 {
			return false
		}
	}
	return true
}

func (g *douyinPlaybackGate) cancelAuth(ctx context.Context, session readerSession, raw json.RawMessage) error {
	var challenge struct {
		RequestID string `json:"requestId"`
	}
	if json.Unmarshal(raw, &challenge) != nil || challenge.RequestID == "" {
		return ErrPlaybackUnavailable
	}
	return session.Call(ctx, g.sessionID, "Fetch.continueWithAuth", map[string]any{"requestId": challenge.RequestID, "authChallengeResponse": map[string]any{"response": "CancelAuth"}}, nil)
}

func (g *douyinPlaybackGate) recordResponse(raw json.RawMessage) {
	var response struct {
		Type     string `json:"type"`
		Response struct {
			URL    string `json:"url"`
			Status int    `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return
	}
	if response.Type == "Media" {
		u, err := url.Parse(response.Response.URL)
		if err == nil && u.Scheme == "https" && u.Host == douyinMediaHost && isExpectedDouyinMediaPath(u.Path) && response.Response.Status >= 200 && response.Response.Status < 300 {
			g.mu.Lock()
			g.mediaSeen = true
			g.mu.Unlock()
		}
		return
	}
	if response.Type != "Document" || response.Response.URL != g.target {
		return
	}
	g.mu.Lock()
	g.status = response.Response.Status
	g.mu.Unlock()
}

func (g *douyinPlaybackGate) recordFrame(raw json.RawMessage) {
	var frame struct {
		Frame struct {
			ID       string `json:"id"`
			URL      string `json:"url"`
			ParentID string `json:"parentId"`
		} `json:"frame"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	g.mu.Lock()
	if frame.Frame.ParentID != "" || frame.Frame.URL != g.target || frame.Frame.ID == "" || frame.Frame.ID != g.frame {
		g.mu.Unlock()
		g.finish(ErrPlaybackUnavailable)
		return
	}
	g.frame = frame.Frame.ID
	g.mu.Unlock()
}

func (g *douyinPlaybackGate) markLoaded(method string, raw json.RawMessage) {
	if method == "Page.lifecycleEvent" {
		var lifecycle struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &lifecycle) != nil || lifecycle.Name != "load" {
			return
		}
	}
	g.mu.Lock()
	valid := g.documentAllowed && g.frame != "" && g.status >= 200 && g.status < 300
	if valid {
		g.loaded = true
	}
	g.mu.Unlock()
	if valid {
		g.finish(nil)
	}
}

func (g *douyinPlaybackGate) waitDocument(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.documentDone:
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.err
	}
}
func (g *douyinPlaybackGate) frameID() string { g.mu.Lock(); defer g.mu.Unlock(); return g.frame }
func (g *douyinPlaybackGate) validate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil || !g.loaded || !g.documentAllowed || !g.embedVerified || !g.detailVerified || !g.mediaSeen {
		return ErrPlaybackUnavailable
	}
	return nil
}
func (g *douyinPlaybackGate) authorizationError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authorization
}

func createPlaybackWorld(ctx context.Context, session readerSession, sessionID, frameID string) (int64, error) {
	var result struct {
		ExecutionContextID int64 `json:"executionContextId"`
	}
	if frameID == "" || session.Call(ctx, sessionID, "Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": "agent-home-station-douyin-playback", "grantUniveralAccess": false}, &result) != nil || result.ExecutionContextID == 0 {
		return 0, ErrPlaybackUnavailable
	}
	return result.ExecutionContextID, nil
}

const remotePlaybackStateExpression = `(() => {
  const videos = Array.from(document.querySelectorAll('video'));
  if (videos.length !== 1) return {present:false};
  const v = videos[0];
  if (!(v instanceof HTMLMediaElement)) return {present:false};
  const key = '__agent_home_station_douyin_player__';
  const sourceKey = '__agent_home_station_douyin_source__';
  const pinned = globalThis[key];
  if (pinned && pinned !== v) return {present:false};
  if (!pinned) globalThis[key] = v;
  const src = String(v.currentSrc || '');
  const knownSource = globalThis[sourceKey];
  if (knownSource && src && knownSource !== src) return {present:false};
  if (src && !knownSource) globalThis[sourceKey] = src;
  const getter = Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype, 'played')?.get;
  if (!getter) return {present:false};
  const played = getter.call(v), ranges=[];
  for(let i=0;i<played.length;i++) ranges.push([played.start(i), played.end(i)]);
  return {present:true,ready:v.readyState>=1,ended:!!v.ended,paused:!!v.paused,duration:Number(v.duration),ranges:ranges};
})()`
const remotePlaybackStartExpression = `(() => { const v=globalThis.__agent_home_station_douyin_player__; if (!(v instanceof HTMLMediaElement) || document.querySelectorAll('video').length!==1 || document.querySelector('video')!==v) return {ok:false}; v.muted=true; v.defaultMuted=true; v.volume=0; return HTMLMediaElement.prototype.play.call(v).then(()=>({ok:true})).catch(()=>({ok:false})); })()`
const remotePlaybackPauseExpression = `(() => { const v=globalThis.__agent_home_station_douyin_player__; if (!(v instanceof HTMLMediaElement) || document.querySelectorAll('video').length!==1 || document.querySelector('video')!==v) return false; HTMLMediaElement.prototype.pause.call(v); return true; })()`

func remotePlaybackState(ctx context.Context, session readerSession, sessionID string, world int64) (observedPlayback, error) {
	var response struct {
		Result struct {
			Type  string           `json:"type"`
			Value observedPlayback `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": remotePlaybackStateExpression, "returnByValue": true, "contextId": world}, &response); err != nil || response.Result.Type != "object" || len(response.ExceptionDetails) != 0 {
		return observedPlayback{}, playbackCallError(err)
	}
	return response.Result.Value, nil
}
func remoteStartPlayback(ctx context.Context, session readerSession, sessionID string, world int64) error {
	var response struct {
		Result struct {
			Type  string `json:"type"`
			Value struct {
				OK bool `json:"ok"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": remotePlaybackStartExpression, "returnByValue": true, "awaitPromise": true, "userGesture": true, "contextId": world}, &response); err != nil || response.Result.Type != "object" || !response.Result.Value.OK {
		return playbackCallError(err)
	}
	return nil
}
func remotePausePlayback(ctx context.Context, session readerSession, sessionID string, world int64) error {
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": remotePlaybackPauseExpression, "returnByValue": true, "contextId": world}, nil); err != nil {
		return playbackCallError(err)
	}
	return nil
}
func remotePlaybackGateError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrPlaybackUnavailable
}
