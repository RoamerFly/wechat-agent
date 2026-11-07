package stationbrowser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// ErrPlaybackUnavailable means Edge could not make the supplied local media
// available for measured playback. A ready sample with no Intervals is not an
// error: it means that the media loaded but did not make playback progress.
var ErrPlaybackUnavailable = errors.New("媒体暂时无法在独立浏览器中播放")

const (
	maxPlaybackMediaBytes = 32 << 20
	minPlaybackSample     = time.Second
	maxPlaybackSample     = 20 * time.Second
	playbackStartupLimit  = 12 * time.Second
)

// PlaybackInterval is an observed HTMLMediaElement.played time range. It is
// never synthesized from duration, wall-clock time, or a seek operation.
type PlaybackInterval struct {
	StartSeconds float64
	EndSeconds   float64
}

// PlaybackSample describes only what the host-owned video element actually
// reported. Ended does not imply a complete watch unless its Intervals cover
// the caller's required range.
type PlaybackSample struct {
	Ready           bool
	Ended           bool
	Paused          bool
	DurationSeconds float64
	Intervals       []PlaybackInterval
}

// SamplePlayback transfers authorized media in bounded CDP chunks into a Blob
// owned by Reader's private Edge instance, then measures actual
// HTMLMediaElement.played ranges. It deliberately accepts bytes rather than
// an arbitrary URL: connected-app adapters cannot make the browser visit a
// signed or remote URL.
func (r *Reader) SamplePlayback(ctx context.Context, media []byte, contentType string, sample time.Duration, authorize func() error) (PlaybackSample, error) {
	if err := ctx.Err(); err != nil {
		return PlaybackSample{}, err
	}
	if r == nil || r.opener == nil || authorize == nil || len(media) == 0 || len(media) > maxPlaybackMediaBytes || !safeMediaType(contentType) || sample < minPlaybackSample || sample > maxPlaybackSample {
		return PlaybackSample{}, ErrPlaybackUnavailable
	}

	// Reader has one private Edge profile and its CDP event stream is exclusive.
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := authorize(); err != nil { // before exposing the bytes
		return PlaybackSample{}, err
	}
	playCtx, cancel := context.WithTimeout(ctx, sample+playbackStartupLimit+8*time.Second)
	defer cancel()

	session, err := r.opener(playCtx, r.profile)
	if err != nil {
		return PlaybackSample{}, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
	}
	defer session.Close()

	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := session.Call(playCtx, "", "Target.createTarget", map[string]any{"url": "about:blank", "background": false}, &created); err != nil || created.TargetID == "" {
		return PlaybackSample{}, playbackCallError(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := session.Call(playCtx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return PlaybackSample{}, playbackCallError(err)
	}
	for _, method := range []string{"Page.enable", "Network.enable", "Runtime.enable"} {
		if err := session.Call(playCtx, attached.SessionID, method, map[string]any{}, nil); err != nil {
			return PlaybackSample{}, playbackCallError(err)
		}
	}
	if err := session.Call(playCtx, attached.SessionID, "Network.setBypassServiceWorker", map[string]any{"bypass": true}, nil); err != nil {
		return PlaybackSample{}, playbackCallError(err)
	}
	if err := session.Call(playCtx, attached.SessionID, "Page.bringToFront", map[string]any{}, nil); err != nil {
		return PlaybackSample{}, playbackCallError(err)
	}
	if err := session.Call(playCtx, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "deny", "eventsEnabled": false}, nil); err != nil {
		return PlaybackSample{}, playbackCallError(err)
	}
	if err := authorize(); err != nil { // immediately before a media request can start
		return PlaybackSample{}, err
	}
	if err := installBlobPlaybackVideo(playCtx, session, attached.SessionID, media, contentType); err != nil {
		return PlaybackSample{}, err
	}
	// Chromium may defer preload in a private headless target. A muted,
	// host-created play request is both the trigger and the evidence boundary;
	// actual progress is still taken only from media.played below.
	if err := startPlayback(playCtx, session, attached.SessionID); err != nil {
		return PlaybackSample{}, err
	}

	deadline := time.NewTimer(playbackStartupLimit)
	defer deadline.Stop()
	for {
		state, err := playbackState(playCtx, session, attached.SessionID)
		if err != nil {
			return PlaybackSample{}, err
		}
		if state.Ready {
			break
		}
		select {
		case <-playCtx.Done():
			return PlaybackSample{}, playCtx.Err()
		case <-deadline.C:
			return PlaybackSample{}, ErrPlaybackUnavailable
		case <-time.After(100 * time.Millisecond):
		}
	}
	// Do not equate a successful play() promise with playback. Keep polling
	// played ranges until the requested actual-playback window elapsed or media
	// ended. There is no seek, and therefore no fabricated span.
	started := time.Now()
	for time.Since(started) < sample {
		select {
		case <-playCtx.Done():
			return PlaybackSample{}, playCtx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		state, err := playbackState(playCtx, session, attached.SessionID)
		if err != nil {
			return PlaybackSample{}, err
		}
		if state.Ended {
			break
		}
	}
	if err := pausePlayback(playCtx, session, attached.SessionID); err != nil {
		return PlaybackSample{}, err
	}
	state, err := playbackState(playCtx, session, attached.SessionID)
	if err != nil {
		return PlaybackSample{}, err
	}
	if err := authorize(); err != nil { // before returning any evidence
		return PlaybackSample{}, err
	}
	return state.toSample(), nil
}

func safeMediaType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	return strings.HasPrefix(value, "video/") || strings.HasPrefix(value, "audio/")
}

const playbackStartExpression = `(() => { const v=document.getElementById('__agent_home_station_playback__'); if (!v) return {ok:false}; return v.play().then(()=>({ok:true})).catch(()=>({ok:false})); })()`
const playbackPauseExpression = `(() => { const v=document.getElementById('__agent_home_station_playback__'); if (!v) return false; v.pause(); return true; })()`
const playbackStateExpression = `(() => { const v=document.getElementById('__agent_home_station_playback__'); if (!v) return {present:false}; const ranges=[]; for(let i=0;i<v.played.length;i++){ ranges.push([v.played.start(i),v.played.end(i)]); } return {present:true,ready:v.readyState>=1,ended:!!v.ended,paused:!!v.paused,duration:Number(v.duration),ranges:ranges}; })()`

// installBlobPlaybackVideo is a fixed host-side protocol, not an adapter
// evaluation API. Each base64 fragment is capped well below the CDP frame
// limit; after construction the video points to a blob: URL, so it cannot
// follow redirects, contact an origin, or expose a local HTTP service.
func installBlobPlaybackVideo(ctx context.Context, session readerSession, sessionID string, media []byte, contentType string) error {
	const chunkSize = 192 << 10
	const begin = `(() => { window.__agent_home_station_media_chunks__=[]; return true; })()`
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": begin, "returnByValue": true}, nil); err != nil {
		return playbackCallError(err)
	}
	for len(media) > 0 {
		n := chunkSize
		if n > len(media) {
			n = len(media)
		}
		encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(media[:n]))
		expression := `window.__agent_home_station_media_chunks__.push(` + string(encoded) + `); true`
		if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, nil); err != nil {
			return playbackCallError(err)
		}
		media = media[n:]
	}
	quotedType, _ := json.Marshal(strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])))
	expression := `(() => { const text=window.__agent_home_station_media_chunks__.join(''); delete window.__agent_home_station_media_chunks__; const raw=atob(text); const out=new Uint8Array(raw.length); for(let i=0;i<raw.length;i++){out[i]=raw.charCodeAt(i);} const prior=document.getElementById('__agent_home_station_playback__'); if(prior){prior.pause(); prior.remove();} const v=document.createElement('video'); v.id='__agent_home_station_playback__'; v.muted=true; v.defaultMuted=true; v.volume=0; v.preload='auto'; v.playsInline=true; v.src=URL.createObjectURL(new Blob([out],{type:` + string(quotedType) + `})); document.body.appendChild(v); return true; })()`
	var result struct {
		Result struct {
			Type  string `json:"type"`
			Value bool   `json:"value"`
		} `json:"result"`
	}
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, &result); err != nil || result.Result.Type != "boolean" || !result.Result.Value {
		return playbackCallError(err)
	}
	return nil
}
func startPlayback(ctx context.Context, session readerSession, sessionID string) error {
	var result struct {
		Result struct {
			Type  string `json:"type"`
			Value struct {
				OK bool `json:"ok"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": playbackStartExpression, "returnByValue": true, "awaitPromise": true, "userGesture": true}, &result); err != nil || result.Result.Type != "object" || !result.Result.Value.OK {
		return playbackCallError(err)
	}
	return nil
}
func pausePlayback(ctx context.Context, session readerSession, sessionID string) error {
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": playbackPauseExpression, "returnByValue": true}, nil); err != nil {
		return playbackCallError(err)
	}
	return nil
}

type observedPlayback struct {
	Present  bool        `json:"present"`
	Ready    bool        `json:"ready"`
	Ended    bool        `json:"ended"`
	Paused   bool        `json:"paused"`
	Duration float64     `json:"duration"`
	Ranges   [][]float64 `json:"ranges"`
}

func playbackState(ctx context.Context, session readerSession, sessionID string) (observedPlayback, error) {
	var response struct {
		Result struct {
			Type  string           `json:"type"`
			Value observedPlayback `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := session.Call(ctx, sessionID, "Runtime.evaluate", map[string]any{"expression": playbackStateExpression, "returnByValue": true}, &response); err != nil || response.Result.Type != "object" || len(response.ExceptionDetails) != 0 || !response.Result.Value.Present {
		return observedPlayback{}, playbackCallError(err)
	}
	return response.Result.Value, nil
}
func (state observedPlayback) toSample() PlaybackSample {
	result := PlaybackSample{Ready: state.Ready, Ended: state.Ended, Paused: state.Paused}
	if isFinitePositive(state.Duration) {
		result.DurationSeconds = state.Duration
	}
	for _, interval := range state.Ranges {
		if len(interval) != 2 || !isFinitePositive(interval[1]) || math.IsNaN(interval[0]) || math.IsInf(interval[0], 0) || interval[0] < 0 || interval[1] <= interval[0] {
			continue
		}
		if result.DurationSeconds > 0 && interval[1] > result.DurationSeconds+0.25 {
			continue
		}
		result.Intervals = append(result.Intervals, PlaybackInterval{StartSeconds: interval[0], EndSeconds: interval[1]})
	}
	return result
}
func isFinitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func playbackCallError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrPlaybackUnavailable
}
