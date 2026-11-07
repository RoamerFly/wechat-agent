package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/sharespec"
)

const ShareReaderToolID = "share-reader"

// ConfiguredChannelsBridge reads only the installed share-reader setting. The
// obsolete native channels configuration is never an authority after install.
func ConfiguredChannelsBridge(ctx context.Context, store *pluginhost.Store) (string, bool, error) {
	configuration, err := store.ConfigurationForExecution(ctx, ShareReaderToolID)
	if err != nil {
		return "", false, errors.New("share_bridge_configuration_unavailable")
	}
	raw, ok := configuration.Values["object_bridge_url"]
	if !ok {
		return "", false, nil
	}
	var configured string
	if json.Unmarshal(raw, &configured) != nil || configured == "" {
		return "", false, errors.New("share_bridge_configuration_invalid")
	}
	base, err := sharespec.ObjectBridgeURL(configured)
	if err != nil {
		return "", false, errors.New("share_bridge_configuration_invalid")
	}
	return base, true, nil
}

func bridgeTimeout(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 25, nil
	}
	var in struct {
		ObjectID       string `json:"object_id"`
		NonceID        string `json:"nonce_id,omitempty"`
		TimeoutSeconds *int64 `json:"timeout_seconds,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		return 0, errors.New("share_bridge_timeout_invalid")
	}
	if in.TimeoutSeconds == nil {
		return 25, nil
	}
	if *in.TimeoutSeconds < 1 || *in.TimeoutSeconds > int64((time.Duration(1<<63-1))/time.Second) {
		return 0, errors.New("share_bridge_timeout_invalid")
	}
	return *in.TimeoutSeconds, nil
}

// CallBoundChannelsBridge makes the single source-bound bridge read. The
// endpoint comes from host configuration and neither it nor response locators
// are exposed in durable task metadata.
func CallBoundChannelsBridge(ctx context.Context, store *pluginhost.Store, binding *BoundChannelsRoute, j pluginhost.Job, raw json.RawMessage, check func(context.Context) error) (json.RawMessage, error) {
	if binding == nil || check == nil {
		return nil, errors.New("share_bridge_unavailable")
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	var in struct {
		ObjectID       string `json:"object_id"`
		NonceID        string `json:"nonce_id,omitempty"`
		TimeoutSeconds *int64 `json:"timeout_seconds,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("share_bridge_source_mismatch")
	}
	base, configured, err := ConfiguredChannelsBridge(ctx, store)
	if err != nil || !configured {
		return nil, errors.New("share_bridge_unavailable")
	}
	expectedTimeout, err := bridgeTimeout(j.Arguments)
	if err != nil {
		return nil, err
	}
	actualTimeout := int64(25)
	if in.TimeoutSeconds != nil {
		actualTimeout = *in.TimeoutSeconds
	}
	if actualTimeout < 1 || actualTimeout > int64((time.Duration(1<<63-1))/time.Second) || actualTimeout != expectedTimeout {
		return nil, errors.New("share_bridge_timeout_invalid")
	}
	binding.mu.Lock()
	err = binding.route.BeginBridge(in.ObjectID, in.NonceID)
	binding.mu.Unlock()
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(actualTimeout) * time.Second
	q := url.Values{"object_id": {in.ObjectID}, "nonce_id": {in.NonceID}}
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/channels/feed/profile?"+q.Encode(), nil)
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("share_bridge_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("share_bridge_rejected")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return nil, errors.New("share_bridge_response_invalid")
	}
	video, err := sharespec.ParseObjectBridgeResponse(body, in.ObjectID)
	if err != nil {
		return nil, errors.New("share_bridge_response_invalid")
	}
	if err = check(ctx); err != nil {
		return nil, err
	}
	binding.mu.Lock()
	err = binding.route.CompleteBridge(base, video)
	binding.mu.Unlock()
	if err != nil {
		return nil, errors.New("share_bridge_response_invalid")
	}
	return json.Marshal(map[string]any{"source_url": base, "feed": json.RawMessage(body)})
}
