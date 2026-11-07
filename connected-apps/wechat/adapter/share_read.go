package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/sharespec"
)

type ChannelsReader interface {
	ChannelsMedia(context.Context, string) (linuxnative.ChannelsSource, error)
}

func IsChannelsShare(message linuxnative.Message) bool {
	return message.Type == 49 && message.AppMessageType != nil && *message.AppMessageType == 51
}

// ShareReadCapability chooses the WeChat-specific resolver for an incoming card.
// Generic public URL reads without a card are authorized separately by the host.
func ShareReadCapability(message linuxnative.Message) (string, error) {
	if IsChannelsShare(message) {
		return "share.resolve_channels", nil
	}
	if message.Type == 49 && message.AppMessageType != nil && (*message.AppMessageType == 4 || *message.AppMessageType == 5) {
		if message.Link != nil && PublicShareURL(message.Link.URL) != "" {
			return "share.resolve_public", nil
		}
		return "", errors.New("share_service_public_source_required")
	}
	return "", errors.New("share_service_source_kind_unsupported")
}

func MatchesChannelsMessage(message linuxnative.Message, account string, source linuxnative.ChannelsSource) bool {
	return IsChannelsShare(message) && message.Direction == "incoming" && account != "" && (message.AccountID == "" || message.AccountID == account) &&
		message.ID != "" && message.ThreadID != "" && source.AccountID == account &&
		source.ThreadID == message.ThreadID && source.MessageID == message.ID
}

// ReadChannelsSource performs one read with the original native message ID.
// Some legacy messages omit AccountID; the caller supplies its bound connection.
func ReadChannelsSource(ctx context.Context, message linuxnative.Message, account string, reader ChannelsReader) (linuxnative.ChannelsSource, error) {
	if !IsChannelsShare(message) || message.Direction != "incoming" || message.ID == "" || message.ThreadID == "" ||
		account == "" || (message.AccountID != "" && message.AccountID != account) {
		return linuxnative.ChannelsSource{}, errors.New("channels_source_scope_mismatch")
	}
	if err := ctx.Err(); err != nil {
		return linuxnative.ChannelsSource{}, err
	}
	source, err := reader.ChannelsMedia(ctx, message.ID)
	if err != nil {
		return linuxnative.ChannelsSource{}, err
	}
	if err := ctx.Err(); err != nil {
		return linuxnative.ChannelsSource{}, err
	}
	if !MatchesChannelsMessage(message, account, source) {
		return linuxnative.ChannelsSource{}, errors.New("channels_source_scope_mismatch")
	}
	return source, nil
}

func RecheckChannelsSource(ctx context.Context, message linuxnative.Message, account string, reader ChannelsReader, original linuxnative.ChannelsSource) error {
	current, err := ReadChannelsSource(ctx, message, account, reader)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || !MatchesChannelsMessage(message, account, original) || current.ObjectID != original.ObjectID || current.ObjectNonceID != original.ObjectNonceID {
		return errors.New("share_native_source_changed")
	}
	return nil
}

func MatchesResolvedChannels(message linuxnative.Message, account string, original, resolved linuxnative.ChannelsSource) bool {
	return MatchesChannelsMessage(message, account, resolved) && resolved.MediaURL != "" &&
		(original.ObjectID == "" || strings.HasPrefix(original.ObjectID, "export/") || resolved.ObjectID == original.ObjectID)
}

// ReadChannelsVideo uses a direct locator first, then the resolver for this
// exact received share. The host still owns network, media probing and access.
func ReadChannelsVideo[T any](ctx context.Context, message linuxnative.Message, account string, reader ChannelsReader,
	read func(context.Context, linuxnative.ChannelsSource) (T, error),
	resolve func(context.Context, linuxnative.ChannelsSource) (linuxnative.ChannelsSource, error),
) (T, error) {
	var zero T
	source, err := ReadChannelsSource(ctx, message, account, reader)
	if err != nil {
		return zero, err
	}
	video, err := read(ctx, source)
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err == nil {
		return video, nil
	}
	resolved, err := resolve(ctx, source)
	if err != nil {
		return zero, err
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if !MatchesResolvedChannels(message, account, source, resolved) {
		return zero, errors.New("share_plugin_handoff_invalid")
	}
	video, err = read(ctx, resolved)
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	return video, err
}

// ChannelsResolutionArguments prepares the parser request for one already
// verified card. The short-link callback is bound to that card by the host.
func ChannelsResolutionArguments(ctx context.Context, source linuxnative.ChannelsSource, hasBridge bool, timeoutSeconds int64,
	shortLink func(context.Context) (string, error),
) (json.RawMessage, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	args := map[string]any{}
	link := ""
	switch {
	case strings.HasPrefix(source.ObjectID, "export/"):
		args["export_id"], args["nonce_id"] = source.ObjectID, source.ObjectNonceID
	case hasBridge && source.ObjectID != "":
		link = "bridge"
		args["object_id"] = source.ObjectID
		// The parser rejects an explicitly empty nonce.
		if source.ObjectNonceID != "" {
			args["nonce_id"] = source.ObjectNonceID
		}
	default:
		if source.ObjectID == "" || shortLink == nil {
			return nil, "", errors.New("share_native_shortlink_required")
		}
		var err error
		link, err = shortLink(ctx)
		if err != nil {
			return nil, "", err
		}
		link, err = sharespec.ShareURL(link)
		if err != nil {
			return nil, "", errors.New("share_native_shortlink_invalid")
		}
		args["url"], args["expected_object_id"] = link, source.ObjectID
	}
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if timeoutSeconds > 0 {
		args["timeout_seconds"] = timeoutSeconds
	}
	raw, err := json.Marshal(args)
	return raw, link, err
}
