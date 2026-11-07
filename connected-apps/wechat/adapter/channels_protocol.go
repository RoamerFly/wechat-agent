package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/sharespec"
	"wechat-bot/internal/strictjson"
)

// ChannelsRoute contains private state for one received video-channel card.
// The host owns locking, authorization, cookies, transport and private result
// storage. This value only validates source-bound parser transitions.
type ChannelsRoute struct {
	Account, Thread, Message, Event string
	Revision                        int
	Object, Nonce                   string
	Phase, Share, Token, Export     string
	Referer                         string
	Resolved                        *linuxnative.ChannelsSource
}

type ChannelsEvent struct {
	Account, Thread, Message, EventID string
	Revision                          int
}

func NewChannelsRoute(message linuxnative.Message, source linuxnative.ChannelsSource, ref ChannelsEvent, verifiedShare string) (ChannelsRoute, error) {
	if message.AccountID == "" || message.ThreadID == "" || message.ID == "" || source.AccountID != message.AccountID || source.ThreadID != message.ThreadID || source.MessageID != message.ID || ref.Account != message.AccountID || ref.Thread != message.ThreadID || ref.Message != message.ID || ref.EventID == "" || ref.Revision < 1 {
		return ChannelsRoute{}, errors.New("share_source_binding_invalid")
	}
	route := ChannelsRoute{Account: message.AccountID, Thread: message.ThreadID, Message: message.ID, Event: ref.EventID, Revision: ref.Revision, Object: source.ObjectID, Nonce: source.ObjectNonceID}
	if verifiedShare == "bridge" {
		if _, err := strconv.ParseUint(route.Object, 10, 64); err != nil || len(route.Nonce) > 4096 {
			return ChannelsRoute{}, errors.New("share_native_shortlink_required")
		}
		route.Phase = "channels-bridge"
		return route, nil
	}
	if verifiedShare != "" {
		share, err := sharespec.ShareURL(verifiedShare)
		if err != nil {
			return ChannelsRoute{}, errors.New("share_source_binding_invalid")
		}
		route.Share, route.Phase = share, "channels-parse"
		return route, nil
	}
	if strings.HasPrefix(route.Object, "export/") && len(route.Object) >= 12 && len(route.Object) <= 4096 && len(route.Nonce) > 0 && len(route.Nonce) <= 4096 {
		route.Phase = "channels-shortlink"
		return route, nil
	}
	return ChannelsRoute{}, errors.New("share_native_shortlink_required")
}

func (route ChannelsRoute) ExpectedRequest(raw json.RawMessage) (string, error) {
	var request struct {
		Route   string          `json:"route"`
		Body    json.RawMessage `json:"body"`
		Referer string          `json:"referer"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Route != route.Phase {
		return "", errors.New("share_route_out_of_order")
	}
	var expected any
	switch route.Phase {
	case "channels-shortlink":
		expected = map[string]any{"exportId": route.Object, "nonceId": route.Nonce, "scene": 40}
	case "channels-parse":
		expected = map[string]any{"type": "video_channel_url", "url": route.Share, "scene": 1}
	case "channels-feed":
		expected = map[string]any{"baseReq": map[string]string{"generalToken": route.Token}, "exportId": route.Export}
	default:
		return "", errors.New("share_route_chain_finished")
	}
	if !strictjson.Matches(request.Body, expected) || (request.Route == "channels-feed" && request.Referer != route.Referer) || (request.Route != "channels-feed" && request.Referer != "") {
		return "", errors.New("share_route_source_mismatch")
	}
	return route.Phase, nil
}

func (route *ChannelsRoute) Accept(phase string, raw json.RawMessage) error {
	if _, err := strictjson.Decode(raw); err != nil {
		return errors.New("share_route_response_invalid")
	}
	fail := func() error { return errors.New("share_route_response_binding_failed") }
	switch phase {
	case "channels-shortlink":
		var response struct {
			Code *int `json:"errCode"`
			Data struct {
				URL string `json:"shortUrl"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Code == nil || *response.Code != 0 {
			return fail()
		}
		share, err := sharespec.ShareURL(response.Data.URL)
		if err != nil {
			return fail()
		}
		route.Share, route.Phase = share, "channels-parse"
	case "channels-parse":
		var response struct {
			Code *int `json:"code"`
			Data struct {
				URL    string `json:"playable_url"`
				Export string `json:"wx_export_id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Code == nil || *response.Code != 0 {
			return fail()
		}
		parsed, err := url.Parse(response.Data.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "channels.weixin.qq.com" || parsed.User != nil || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/finder-preview/") {
			return fail()
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil || len(query["token"]) != 1 || len(query["eid"]) > 1 {
			return fail()
		}
		token, export := query.Get("token"), query.Get("eid")
		if export == "" {
			export = response.Data.Export
		}
		if token == "" || len(token) > 8192 || export == "" || len(export) > 4096 {
			return fail()
		}
		if strings.HasPrefix(route.Object, "export/") && export != route.Object {
			return fail()
		}
		route.Token, route.Export, route.Referer, route.Phase = token, export, parsed.String(), "channels-feed"
	case "channels-feed":
		var response struct {
			Code *int `json:"errCode"`
			Data struct {
				Scene struct {
					Export json.RawMessage `json:"dynamicExportId"`
				} `json:"sceneInfo"`
				Author struct {
					Name string `json:"nickname"`
				} `json:"authorInfo"`
				Feed struct {
					ID    json.RawMessage `json:"objectId"`
					Key   json.RawMessage `json:"decodeKey"`
					URL   string          `json:"videoUrl"`
					Title string          `json:"description"`
					H264  struct {
						URL string `json:"videoUrl"`
					} `json:"h264VideoInfo"`
				} `json:"feedInfo"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Code == nil || *response.Code != 0 {
			return fail()
		}
		export := ""
		if len(response.Data.Scene.Export) > 0 {
			if json.Unmarshal(response.Data.Scene.Export, &export) != nil || export == "" || export != route.Export {
				return fail()
			}
		}
		id, err := channelUint64(response.Data.Feed.ID, true)
		if len(response.Data.Feed.ID) == 0 {
			if export == "" {
				return fail()
			}
			nativeID, _ := json.Marshal(route.Object)
			id, err = channelUint64(nativeID, true)
		}
		if err != nil {
			return fail()
		}
		if route.Object != "" && !strings.HasPrefix(route.Object, "export/") && id != route.Object {
			return fail()
		}
		key, err := channelUint64(response.Data.Feed.Key, false)
		if err != nil {
			return fail()
		}
		locator := response.Data.Feed.H264.URL
		if locator == "" {
			locator = response.Data.Feed.URL
		}
		parsed, err := url.Parse(locator)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || len(locator) > 16384 || strings.ContainsAny(locator, "\\\r\n\x00") {
			return fail()
		}
		route.Resolved = &linuxnative.ChannelsSource{AccountID: route.Account, ThreadID: route.Thread, MessageID: route.Message, ObjectID: id, MediaURL: locator, DecodeKey: key, Title: response.Data.Feed.Title, Author: response.Data.Author.Name, State: "available"}
		route.Phase = "complete"
	default:
		return fail()
	}
	return nil
}

func (route *ChannelsRoute) BeginBridge(object, nonce string) error {
	if object != route.Object || nonce != route.Nonce {
		return errors.New("share_bridge_source_mismatch")
	}
	if route.Phase != "channels-bridge" {
		return errors.New("share_bridge_already_consumed")
	}
	route.Phase = "bridge-reading"
	return nil
}

func (route *ChannelsRoute) CompleteBridge(base string, video sharespec.ResolvedVideo) error {
	if route.Phase != "bridge-reading" || video.ObjectID != route.Object {
		return errors.New("share_bridge_response_invalid")
	}
	route.Share, route.Resolved, route.Phase = base, &linuxnative.ChannelsSource{AccountID: route.Account, ThreadID: route.Thread, MessageID: route.Message, ObjectID: video.ObjectID, MediaURL: video.URL, DecodeKey: video.DecodeKey, Title: video.Title, Author: video.Author, State: "available"}, "complete"
	return nil
}

func channelUint64(raw json.RawMessage, required bool) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if required {
			return "", errors.New("missing identity")
		}
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		value = string(raw)
	}
	if value == "" || len(value) > 20 {
		return "", errors.New("invalid identity")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return "", errors.New("invalid identity")
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return "", errors.New("invalid identity")
	}
	return value, nil
}

func (route ChannelsRoute) ValidateResult(raw json.RawMessage) ([]byte, linuxnative.ChannelsSource, error) {
	fail := func() ([]byte, linuxnative.ChannelsSource, error) {
		return nil, linuxnative.ChannelsSource{}, errors.New("share_result_binding_failed")
	}
	if route.Phase != "complete" || route.Resolved == nil {
		return fail()
	}
	if _, err := strictjson.Decode(raw); err != nil {
		return fail()
	}
	var result struct {
		OK         *bool    `json:"ok"`
		Platform   string   `json:"platform"`
		Source     string   `json:"source_url"`
		Object     string   `json:"object_id"`
		Title      string   `json:"title"`
		Author     string   `json:"author"`
		Locators   []string `json:"private_media_locators"`
		Key        string   `json:"private_decode_key"`
		Coverage   string   `json:"coverage"`
		Observed   *bool    `json:"video_observed"`
		Downloaded *bool    `json:"media_downloaded"`
		Untrusted  *bool    `json:"untrusted_external_content"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&result) != nil {
		return fail()
	}
	p := route.Resolved
	if result.OK == nil || !*result.OK || result.Platform != "channels" || result.Source != route.Share || result.Object != p.ObjectID || result.Title != p.Title || result.Author != p.Author || len(result.Locators) != 1 || result.Locators[0] != p.MediaURL || result.Key != p.DecodeKey || result.Coverage != "media_locator_only" || result.Observed == nil || *result.Observed || result.Downloaded == nil || *result.Downloaded || result.Untrusted == nil || !*result.Untrusted {
		return fail()
	}
	checkpoint, _ := json.Marshal(map[string]any{"ok": true, "platform": "channels", "source_url": route.Share, "object_id": p.ObjectID, "title": p.Title, "author": p.Author, "coverage": "media_locator_only", "video_observed": false, "media_downloaded": false, "untrusted_external_content": true})
	return checkpoint, *p, nil
}
