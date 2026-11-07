//go:build windows

package stationbrowser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"sync"
	"time"

	"wechat-bot/internal/hostfile"
)

const maxDouyinWebDetailBytes = 256 << 10

// SampleDouyinWebVideo uses only the caller's dedicated saved login profile.
// It proves measured playback, not understanding, and returns no signed media
// URLs. A direct media source must match the exact video's observed detail
// response; blob/MSE sources without that mapping are deliberately unsupported.
func SampleDouyinWebVideo(ctx context.Context, profile, videoID string, sample time.Duration, authorize func() error) (PlaybackSample, error) {
	target, err := DouyinVideoURL(videoID)
	if err != nil {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}
	return sampleDouyinWebVideo(ctx, profile, target, videoID, sample, authorize, false)
}

// localFixture is private and used only for localhost synthetic acceptance.
func sampleDouyinWebVideo(ctx context.Context, profile, target, videoID string, sample time.Duration, authorize func() error, localFixture bool) (PlaybackSample, error) {
	if err := ctx.Err(); err != nil {
		return PlaybackSample{}, err
	}
	if authorize == nil || !douyinVideoID.MatchString(videoID) || sample < minPlaybackSample || sample > maxPlaybackSample || !validDouyinWebPlaybackTarget(target, videoID, localFixture) {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}
	if err := authorize(); err != nil {
		return PlaybackSample{}, err
	}
	lock, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}
	release, err := hostfile.TryLock(lock)
	if err != nil {
		return PlaybackSample{}, ErrDouyinLoginActive
	}
	defer release()
	playCtx, cancel := context.WithTimeout(ctx, sample+playbackStartupLimit+8*time.Second)
	defer cancel()
	var session *cdpSession
	if localFixture {
		session, err = openCDP(playCtx, profile)
	} else {
		session, err = openRestrictedCDP(playCtx, profile, isDouyinHomeHost)
	}
	if err != nil {
		return PlaybackSample{}, ErrBrowserUnavailable
	}
	defer closeDouyinHomeSession(session)
	result, err := sampleDouyinWebSession(playCtx, session, target, videoID, sample, authorize, localFixture)
	if playCtx.Err() != nil {
		return PlaybackSample{}, playCtx.Err()
	}
	return result, err
}

func validDouyinWebPlaybackTarget(raw, videoID string, fixture bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.EscapedPath() != "/video/"+videoID {
		return false
	}
	if fixture {
		return u.Scheme == "http" && net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()
	}
	return u.Scheme == "https" && u.Host == "www.douyin.com"
}

func sampleDouyinWebSession(ctx context.Context, session *cdpSession, target, videoID string, sample time.Duration, authorize func() error, fixture bool) (PlaybackSample, error) {
	page, err := openDouyinWebDetailPage(ctx, session, target, videoID, authorize, fixture)
	if err != nil {
		return PlaybackSample{}, err
	}
	defer page.listener.stop()
	world, err := createPlaybackWorld(ctx, session, page.sessionID, page.frameID)
	if err != nil {
		return PlaybackSample{}, err
	}
	ready := time.NewTimer(playbackStartupLimit)
	defer ready.Stop()
	var state observedPlayback
	for {
		if err := authorize(); err != nil {
			return PlaybackSample{}, err
		}
		state, err = douyinWebPlaybackState(ctx, session, page.sessionID, world, target, page.urls)
		if err != nil {
			return PlaybackSample{}, err
		}
		// A private headless target may defer preload until play(). Presence
		// already requires the exact detail-bound native currentSrc; readiness
		// is measured after the host's muted play request.
		if state.Present {
			break
		}
		select {
		case <-ctx.Done():
			return PlaybackSample{}, ctx.Err()
		case <-ready.C:
			return PlaybackSample{}, ErrPlaybackUnavailable
		case <-time.After(80 * time.Millisecond):
		}
	}
	if err := douyinWebPlaybackAction(ctx, session, page.sessionID, world, target, page.urls, true); err != nil {
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
		if _, err := page.listener.result(); err != nil {
			return PlaybackSample{}, err
		}
		state, err = douyinWebPlaybackState(ctx, session, page.sessionID, world, target, page.urls)
		if err != nil || !state.Present {
			return PlaybackSample{}, ErrPlaybackUnavailable
		}
		if state.Ended {
			break
		}
	}
	if err := douyinWebPlaybackAction(ctx, session, page.sessionID, world, target, page.urls, false); err != nil {
		return PlaybackSample{}, err
	}
	state, err = douyinWebPlaybackState(ctx, session, page.sessionID, world, target, page.urls)
	if err != nil || !state.Present {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}
	if err := authorize(); err != nil {
		return PlaybackSample{}, err
	}
	if _, err := page.listener.result(); err != nil {
		return PlaybackSample{}, err
	}
	return state.toSample(), nil
}

type douyinWebDetailPage struct {
	sessionID, frameID string
	urls               []string
	listener           *douyinWebDetailListener
}

// Navigation and exact-video metadata are common to measured playback and
// audio-source retrieval. Neither path accepts arbitrary caller-supplied URLs.
func openDouyinWebDetailPage(ctx context.Context, session *cdpSession, target, videoID string, authorize func() error, fixture bool) (douyinWebDetailPage, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	// This target is active only inside a windowless, private Edge instance.
	// A background target can defer media loading indefinitely; no OS window
	// or keyboard/mouse focus is created or activated by this operation.
	if session.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": false}, &created) != nil || created.TargetID == "" {
		return douyinWebDetailPage{}, ErrPlaybackUnavailable
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached) != nil || attached.SessionID == "" {
		return douyinWebDetailPage{}, ErrPlaybackUnavailable
	}
	for _, call := range []struct {
		method string
		params any
	}{
		{"Page.enable", map[string]any{}},
		{"Network.enable", map[string]any{}},
		{"Runtime.enable", map[string]any{}},
		{"Network.setBypassServiceWorker", map[string]any{"bypass": true}},
		{"Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": remotePlaybackPrelude}},
	} {
		if session.Call(ctx, attached.SessionID, call.method, call.params, nil) != nil {
			return douyinWebDetailPage{}, ErrPlaybackUnavailable
		}
	}
	listener := newDouyinWebDetailListener(target, videoID, attached.SessionID, fixture)
	listener.start(ctx, session, authorize)
	keep := false
	defer func() {
		if !keep {
			listener.stop()
		}
	}()
	if err := authorize(); err != nil {
		return douyinWebDetailPage{}, err
	}
	var nav struct {
		FrameID    string `json:"frameId"`
		ErrorText  string `json:"errorText"`
		IsDownload bool   `json:"isDownload"`
	}
	if session.Call(ctx, attached.SessionID, "Page.navigate", map[string]any{"url": target}, &nav) != nil || nav.FrameID == "" || nav.ErrorText != "" || nav.IsDownload {
		return douyinWebDetailPage{}, ErrPlaybackUnavailable
	}
	var urls []string
	ready := time.NewTimer(playbackStartupLimit)
	defer ready.Stop()
	for len(urls) == 0 {
		if err := authorize(); err != nil {
			return douyinWebDetailPage{}, err
		}
		var err error
		urls, err = listener.result()
		if err != nil {
			return douyinWebDetailPage{}, err
		}
		if len(urls) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			return douyinWebDetailPage{}, ctx.Err()
		case <-ready.C:
			return douyinWebDetailPage{}, ErrPlaybackUnavailable
		case <-time.After(80 * time.Millisecond):
		}
	}
	keep = true
	return douyinWebDetailPage{sessionID: attached.SessionID, frameID: nav.FrameID, urls: urls, listener: listener}, nil
}

type douyinWebDetailListener struct {
	target, videoID, session string
	fixture                  bool
	mu                       sync.Mutex
	urls                     []string
	err                      error
	cancel                   context.CancelFunc
	done                     chan struct{}
}

func newDouyinWebDetailListener(target, videoID, session string, fixture bool) *douyinWebDetailListener {
	return &douyinWebDetailListener{target: target, videoID: videoID, session: session, fixture: fixture, done: make(chan struct{})}
}
func (g *douyinWebDetailListener) result() ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.urls...), g.err
}
func (g *douyinWebDetailListener) fail(err error) {
	g.mu.Lock()
	if g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
}
func (g *douyinWebDetailListener) stop() { g.cancel(); <-g.done }
func (g *douyinWebDetailListener) start(parent context.Context, session readerSession, authorize func() error) {
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	go func() {
		defer close(g.done)
		pending := map[string]bool{}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-session.Events():
				if !ok {
					g.fail(ErrPlaybackUnavailable)
					return
				}
				if event.SessionID != g.session {
					continue
				}
				if err := authorize(); err != nil {
					g.fail(err)
					return
				}
				switch event.Method {
				case "Page.frameNavigated":
					var data struct {
						Frame struct {
							URL    string `json:"url"`
							Parent string `json:"parentId"`
						} `json:"frame"`
					}
					if json.Unmarshal(event.Params, &data) == nil && data.Frame.Parent == "" && data.Frame.URL != "about:blank" && !sameDouyinHomeLocation(data.Frame.URL, g.target) {
						g.fail(ErrPlaybackUnavailable)
						return
					}
				case "Network.responseReceived":
					var data struct {
						RequestID string `json:"requestId"`
						Type      string `json:"type"`
						Response  struct {
							URL               string `json:"url"`
							Status            int    `json:"status"`
							FromServiceWorker bool   `json:"fromServiceWorker"`
						} `json:"response"`
					}
					if json.Unmarshal(event.Params, &data) == nil && (data.Type == "XHR" || data.Type == "Fetch") && data.Response.Status == 200 && !data.Response.FromServiceWorker && g.detailURL(data.Response.URL) {
						pending[data.RequestID] = true
					}
				case "Network.loadingFinished":
					var data struct {
						RequestID string  `json:"requestId"`
						Length    float64 `json:"encodedDataLength"`
					}
					if json.Unmarshal(event.Params, &data) != nil || !pending[data.RequestID] {
						continue
					}
					delete(pending, data.RequestID)
					if data.Length > maxDouyinWebDetailBytes {
						g.fail(ErrPlaybackUnavailable)
						return
					}
					var body struct {
						Body   string `json:"body"`
						Base64 bool   `json:"base64Encoded"`
					}
					if session.Call(ctx, g.session, "Network.getResponseBody", map[string]any{"requestId": data.RequestID}, &body) != nil {
						g.fail(ErrPlaybackUnavailable)
						return
					}
					raw := []byte(body.Body)
					if body.Base64 {
						var err error
						raw, err = base64.StdEncoding.DecodeString(body.Body)
						if err != nil {
							g.fail(ErrPlaybackUnavailable)
							return
						}
					}
					urls, err := g.parseDetail(raw)
					if err != nil {
						g.fail(err)
						return
					}
					g.mu.Lock()
					g.urls = urls
					g.mu.Unlock()
				}
			}
		}
	}()
}

func (g *douyinWebDetailListener) detailURL(raw string) bool {
	u, err := url.Parse(raw)
	target, _ := url.Parse(g.target)
	if err != nil || u.User != nil || u.Fragment != "" || u.EscapedPath() != douyinDetailPath || u.Scheme != target.Scheme || u.Host != target.Host {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	return err == nil && len(q["aweme_id"]) == 1 && q.Get("aweme_id") == g.videoID
}
func (g *douyinWebDetailListener) parseDetail(raw []byte) ([]string, error) {
	if len(raw) == 0 || len(raw) > maxDouyinWebDetailBytes {
		return nil, ErrPlaybackUnavailable
	}
	type address struct {
		URLs []string `json:"url_list"`
	}
	var data struct {
		Status int `json:"status_code"`
		Detail struct {
			ID    string `json:"aweme_id"`
			Video struct {
				Play  address `json:"play_addr"`
				H264  address `json:"play_addr_h264"`
				H265  address `json:"play_addr_h265"`
				Rates []struct {
					Play address `json:"play_addr"`
				} `json:"bit_rate"`
			} `json:"video"`
		} `json:"aweme_detail"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Status != 0 || data.Detail.ID != g.videoID {
		return nil, ErrPlaybackUnavailable
	}
	all := append(append(append([]string{}, data.Detail.Video.Play.URLs...), data.Detail.Video.H264.URLs...), data.Detail.Video.H265.URLs...)
	for _, rate := range data.Detail.Video.Rates {
		all = append(all, rate.Play.URLs...)
	}
	seen := map[string]bool{}
	urls := []string{}
	for _, rawURL := range all {
		u, err := url.Parse(rawURL)
		if err != nil || len(rawURL) > 8192 || u.User != nil || u.Fragment != "" || u.Opaque != "" {
			return nil, ErrPlaybackUnavailable
		}
		valid := u.Scheme == "https" && u.Port() == "" && isDouyinHomeHost(u.Host)
		if g.fixture {
			target, _ := url.Parse(g.target)
			valid = u.Scheme == "http" && u.Host == target.Host
		}
		if !valid {
			return nil, ErrPlaybackUnavailable
		}
		if !seen[rawURL] {
			urls = append(urls, rawURL)
			seen[rawURL] = true
		}
	}
	if len(urls) == 0 {
		return nil, ErrPlaybackUnavailable
	}
	return urls, nil
}

// Runs in an isolated world, so page scripts cannot replace native getters or
// play/pause methods. The pinned element and media URL cannot change mid-sample.
func douyinWebPlaybackExpression(target string, sources []string) string {
	targetJSON, _ := json.Marshal(target)
	urlsJSON, _ := json.Marshal(sources)
	return `(() => {
 const expected=` + string(targetJSON) + `, sources=` + string(urlsJSON) + `;
 if(String(location.href)!==expected)return {invalid:true};
 const visible=e=>{const r=e.getBoundingClientRect(),s=getComputedStyle(e);return e.getClientRects().length&&r.width>0&&r.height>0&&r.bottom>0&&r.right>0&&r.top<innerHeight&&r.left<innerWidth&&s.visibility==='visible'&&s.display!=='none'&&Number(s.opacity)>0};
 if([...document.querySelectorAll('input[type=password],form[action*="login"],form[action*="passport"],iframe[src*="captcha"],[class*="captcha"],[class*="verify"]')].some(visible))return {login:true};
 const videos=[...document.querySelectorAll('video')].filter(visible);
 if(videos.length>1)return {invalid:true}; if(videos.length===0)return {present:false};
 const v=videos[0],get=p=>Object.getOwnPropertyDescriptor(HTMLMediaElement.prototype,p).get.call(v);
 const src=String(get('currentSrc')||''); if(!src)return {present:false}; if(!sources.includes(src))return {invalid:true};
 const pinned=globalThis.__agent_home_station_douyin_player__, prior=globalThis.__agent_home_station_douyin_source__;
 if(pinned&&pinned!==v||prior&&prior!==src)return {invalid:true};
 globalThis.__agent_home_station_douyin_player__=v; globalThis.__agent_home_station_douyin_source__=src;
 const p=get('played'),ranges=[]; for(let i=0;i<p.length;i++)ranges.push([p.start(i),p.end(i)]);
 return {present:true,ready:get('readyState')>=1,ended:!!get('ended'),paused:!!get('paused'),duration:Number(get('duration')),ranges};
})()`
}
func douyinWebPlaybackState(ctx context.Context, session readerSession, id string, world int64, target string, sources []string) (observedPlayback, error) {
	var response struct {
		Result struct {
			Type  string `json:"type"`
			Value struct {
				observedPlayback
				Invalid bool `json:"invalid"`
				Login   bool `json:"login"`
			} `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	err := session.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": douyinWebPlaybackExpression(target, sources), "returnByValue": true, "contextId": world}, &response)
	if err != nil || response.Result.Type != "object" || len(response.Exception) != 0 || response.Result.Value.Invalid {
		return observedPlayback{}, ErrPlaybackUnavailable
	}
	if response.Result.Value.Login {
		return observedPlayback{}, ErrDouyinWebLoginRequired
	}
	return response.Result.Value.observedPlayback, nil
}

// Ordinary pages may preload hidden recommendations. Start and pause only the
// exact visible element already pinned by the source-checked state read.
func douyinWebPlaybackAction(ctx context.Context, session readerSession, id string, world int64, target string, sources []string, start bool) error {
	operation := `HTMLMediaElement.prototype.pause.call(v);`
	if start {
		operation = `v.muted=true;v.defaultMuted=true;v.volume=0;await HTMLMediaElement.prototype.play.call(v);`
	}
	expression := `(async()=>{ const s=` + douyinWebPlaybackExpression(target, sources) + `;if(!s.present||s.invalid||s.login)return {ok:false};const v=globalThis.__agent_home_station_douyin_player__;try{` + operation + `return {ok:true}}catch{return {ok:false}}})()`
	var result struct {
		Result struct {
			Type  string `json:"type"`
			Value struct {
				OK bool `json:"ok"`
			} `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if session.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": expression, "contextId": world, "returnByValue": true, "awaitPromise": true, "userGesture": start}, &result) != nil || result.Result.Type != "object" || !result.Result.Value.OK || len(result.Exception) != 0 {
		return ErrPlaybackUnavailable
	}
	return nil
}
