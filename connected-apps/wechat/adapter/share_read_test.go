package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"wechat-bot/internal/linuxnative"
)

type channelsLookup func(context.Context, string) (linuxnative.ChannelsSource, error)

func (f channelsLookup) ChannelsMedia(ctx context.Context, id string) (linuxnative.ChannelsSource, error) {
	return f(ctx, id)
}

func TestReadChannelsVideoKeepsOriginalShareOnFallback(t *testing.T) {
	kind := 51
	message := linuxnative.Message{AccountID: "account", ThreadID: "same-room", ID: "target-card", Direction: "incoming", Type: 49, AppMessageType: &kind}
	target := linuxnative.ChannelsSource{AccountID: message.AccountID, ThreadID: message.ThreadID, MessageID: message.ID, ObjectID: "202"}
	for _, change := range []string{"none", "old-message", "old-object", "other-account", "cancel"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			source := channelsLookup(func(_ context.Context, id string) (linuxnative.ChannelsSource, error) {
				calls = append(calls, "native:"+id)
				return target, nil
			})
			got, err := ReadChannelsVideo(ctx, message, message.AccountID, source,
				func(_ context.Context, current linuxnative.ChannelsSource) (string, error) {
					calls = append(calls, "read:"+current.MessageID)
					if current.MediaURL == "" {
						return "", errors.New("unavailable direct locator")
					}
					return "target content", nil
				},
				func(_ context.Context, current linuxnative.ChannelsSource) (linuxnative.ChannelsSource, error) {
					calls = append(calls, "resolve:"+current.MessageID)
					current.MediaURL = "https://synthetic.example/video"
					switch change {
					case "old-message":
						current.MessageID = "older-card"
					case "old-object":
						current.ObjectID = "101"
					case "other-account":
						current.AccountID = "other"
					case "cancel":
						cancel()
					}
					return current, nil
				},
			)
			want := []string{"native:target-card", "read:target-card", "resolve:target-card"}
			if change == "none" {
				want = append(want, "read:target-card")
				if err != nil || got != "target content" {
					t.Fatal(got, err)
				}
			} else if err == nil || got != "" {
				t.Fatal("foreign or cancelled locator produced content", got, err)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls, want)
			}
		})
	}
}

func TestChannelsNativeObjectAndRouteArgumentsStayBound(t *testing.T) {
	kind := 51
	message := linuxnative.Message{AccountID: "account", ThreadID: "room", ID: "card", Direction: "incoming", Type: 49, AppMessageType: &kind}
	original := linuxnative.ChannelsSource{AccountID: "account", ThreadID: "room", MessageID: "card", ObjectID: "42", ObjectNonceID: "original"}
	if err := RecheckChannelsSource(t.Context(), message, "account", channelsLookup(func(_ context.Context, id string) (linuxnative.ChannelsSource, error) {
		if id != message.ID {
			t.Fatal(id)
		}
		changed := original
		changed.ObjectNonceID = "replacement"
		return changed, nil
	}), original); err == nil || err.Error() != "share_native_source_changed" {
		t.Fatal(err)
	}
	const short = "https://weixin.qq.com/sph/synthetic123"
	for _, tc := range []struct {
		name, object, nonce, link, args string
		bridge                          bool
		shortCalls                      int
	}{
		{"native link", "42", "original", short, `{"expected_object_id":"42","timeout_seconds":75,"url":"https://weixin.qq.com/sph/synthetic123"}`, false, 1},
		{"numeric bridge", "42", "", "bridge", `{"object_id":"42","timeout_seconds":75}`, true, 0},
		{"exact export", "export/original", "nonce", "", `{"export_id":"export/original","nonce_id":"nonce","timeout_seconds":75}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := original
			source.ObjectID, source.ObjectNonceID = tc.object, tc.nonce
			calls := 0
			args, link, err := ChannelsResolutionArguments(t.Context(), source, tc.bridge, 75, func(context.Context) (string, error) {
				calls++
				return short, nil
			})
			if err != nil || link != tc.link || string(args) != tc.args || calls != tc.shortCalls || !json.Valid(args) {
				t.Fatal(string(args), link, calls, err)
			}
		})
	}
}
