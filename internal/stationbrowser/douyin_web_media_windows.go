//go:build windows

package stationbrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"wechat-bot/internal/hostfile"
)

func ReadDouyinWebMedia(ctx context.Context, profile, videoID string, authorize func() error) (DouyinWebMedia, error) {
	target, err := DouyinVideoURL(videoID)
	if err != nil {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	return readDouyinWebMedia(ctx, profile, target, videoID, authorize, false, nil)
}

func readDouyinWebMedia(ctx context.Context, profile, target, videoID string, authorize func() error, fixture bool, fixtureClient *http.Client) (DouyinWebMedia, error) {
	if err := ctx.Err(); err != nil {
		return DouyinWebMedia{}, err
	}
	if authorize == nil || !douyinVideoID.MatchString(videoID) || !validDouyinWebPlaybackTarget(target, videoID, fixture) {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	if err := authorize(); err != nil {
		return DouyinWebMedia{}, err
	}
	lock, err := DouyinLoginProfileLockPath(profile)
	if err != nil {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	release, err := hostfile.TryLock(lock)
	if err != nil {
		return DouyinWebMedia{}, ErrDouyinLoginActive
	}
	defer release()
	readCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var session *cdpSession
	if fixture {
		session, err = openCDP(readCtx, profile)
	} else {
		session, err = openRestrictedCDP(readCtx, profile, isDouyinHomeHost)
	}
	if err != nil {
		return DouyinWebMedia{}, ErrBrowserUnavailable
	}
	defer closeDouyinHomeSession(session)
	page, err := openDouyinWebDetailPage(readCtx, session, target, videoID, authorize, fixture)
	if err != nil {
		if errors.Is(err, ErrPlaybackUnavailable) {
			err = ErrDouyinWebMediaUnavailable
		}
		if readCtx.Err() != nil {
			err = readCtx.Err()
		}
		return DouyinWebMedia{}, err
	}
	defer page.listener.stop()
	world, err := createPlaybackWorld(readCtx, session, page.sessionID, page.frameID)
	if err != nil {
		return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
	}
	verify := func() error {
		if err := authorize(); err != nil {
			return err
		}
		if _, err := page.listener.result(); err != nil {
			if errors.Is(err, ErrPlaybackUnavailable) {
				return ErrDouyinWebMediaUnavailable
			}
			return err
		}
		return validateDouyinWebMediaPage(readCtx, session, page.sessionID, world, target)
	}
	if err := verify(); err != nil {
		return DouyinWebMedia{}, err
	}
	client := fixtureClient
	allowURL := validDouyinWebMediaURL
	if fixture {
		if client == nil {
			client = &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true}}
		}
		allowURL = func(raw string) bool { return validLocalDouyinMediaURL(raw, target) }
	} else {
		var cleanup func()
		client, cleanup, err = newDouyinWebMediaClient(readCtx)
		if err != nil {
			return DouyinWebMedia{}, err
		}
		defer cleanup()
	}
	tooLarge := false
	for _, candidate := range page.urls {
		if err := verify(); err != nil {
			return DouyinWebMedia{}, err
		}
		media, err := fetchDouyinWebMedia(readCtx, candidate, client, verify, allowURL)
		if err == nil {
			if err := verify(); err != nil {
				return DouyinWebMedia{}, err
			}
			return media, nil
		}
		if readCtx.Err() != nil {
			return DouyinWebMedia{}, readCtx.Err()
		}
		if err := verify(); err != nil {
			return DouyinWebMedia{}, err
		}
		if !errors.Is(err, ErrDouyinWebMediaUnavailable) && !errors.Is(err, ErrDouyinWebMediaTooLarge) {
			return DouyinWebMedia{}, err
		}
		tooLarge = tooLarge || errors.Is(err, ErrDouyinWebMediaTooLarge)
	}
	if tooLarge {
		return DouyinWebMedia{}, ErrDouyinWebMediaTooLarge
	}
	return DouyinWebMedia{}, ErrDouyinWebMediaUnavailable
}

func validateDouyinWebMediaPage(ctx context.Context, session readerSession, id string, world int64, target string) error {
	expected, _ := json.Marshal(target)
	expression := `(()=>{if(String(location.href)!==` + string(expected) + `)return {valid:false};const visible=e=>{const r=e.getBoundingClientRect(),s=getComputedStyle(e);return e.getClientRects().length&&r.width>0&&r.height>0&&s.visibility==='visible'&&s.display!=='none'&&Number(s.opacity)>0};return {valid:true,login:[...document.querySelectorAll('input[type=password],form[action*="login"],form[action*="passport"],iframe[src*="captcha"],[class*="captcha"],[class*="verify"]')].some(visible)}})()`
	var result struct {
		Result struct {
			Type  string `json:"type"`
			Value struct {
				Valid bool `json:"valid"`
				Login bool `json:"login"`
			} `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if session.Call(ctx, id, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "contextId": world}, &result) != nil || result.Result.Type != "object" || len(result.Exception) != 0 || !result.Result.Value.Valid {
		return ErrDouyinWebMediaUnavailable
	}
	if result.Result.Value.Login {
		return ErrDouyinWebLoginRequired
	}
	return nil
}

func validLocalDouyinMediaURL(raw, target string) bool {
	u, err := url.Parse(raw)
	origin, originErr := url.Parse(target)
	return err == nil && originErr == nil && u.Scheme == "http" && u.Host == origin.Host && u.User == nil && u.Fragment == "" && u.Opaque == "" && len(raw) <= 8192
}
