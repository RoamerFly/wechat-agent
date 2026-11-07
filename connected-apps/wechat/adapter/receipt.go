package adapter

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
)

type wechatReceiptReader interface {
	LookupSend(context.Context, string) (linuxnative.ToolStatus, error)
}

// LookupDeliveryReceipt checks a stored receipt and optionally refreshes its original key.
func LookupDeliveryReceipt(ctx context.Context, native any, thread, source string, stored linuxnative.ToolStatus, byJob bool, result *apphost.DeliveryReceipt) {
	ProjectDeliveryReceipt(result, stored)
	if result.State == "accepted" || (result.State == "not_submitted" && stored.ID == "") {
		return
	}
	if err := ctx.Err(); err != nil {
		result.Lookup = "unavailable"
		return
	}
	var lookup func(context.Context, string) (linuxnative.ToolStatus, error)
	key := result.IdempotencyKey
	if byJob {
		key = stored.ID
		if reader, ok := native.(interface {
			ToolSendStatus(context.Context, string) (linuxnative.ToolStatus, error)
		}); ok {
			lookup = reader.ToolSendStatus
		}
	} else if reader, ok := native.(wechatReceiptReader); ok {
		lookup = reader.LookupSend
	}
	if key == "" {
		return
	}
	if lookup == nil {
		result.Lookup = "unavailable"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, err := lookup(ctx, key)
	switch {
	case errors.Is(err, linuxnative.ErrToolNotFound):
		result.Lookup = "not_found"
	case err != nil:
		result.Lookup = "unavailable"
	case !identity.ValidPathID(status.ID) || status.ThreadID != thread || (status.Kind != "" && status.Kind != "send" && status.Kind != result.Kind) ||
		(stored.ID != "" && status.ID != stored.ID) || (source != "" && status.SourceID != "" && status.SourceID != source):
		result.Lookup = "identity_mismatch"
	default:
		result.Lookup = "platform"
		ProjectDeliveryReceipt(result, status)
	}
}

var wechatResultCode = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

func validResultCode(code string) bool { return wechatResultCode.MatchString(code) }

// ProjectDeliveryReceipt retains accepted segment evidence while projecting platform state.
func ProjectDeliveryReceipt(out *apphost.DeliveryReceipt, status linuxnative.ToolStatus) {
	out.State, out.OperationID = "submission_unknown", status.ID
	out.CancelRequested = out.CancelRequested || status.CancelRequested
	out.PlatformState, out.PlatformCode, out.ServerID = "", "", ""
	if validResultCode(status.State) {
		out.PlatformState = status.State
	}
	if validResultCode(status.ErrorCode) {
		out.PlatformCode = status.ErrorCode
	} else if validResultCode(status.Reason) {
		out.PlatformCode = status.Reason
	}
	accepted := make(map[int]string, len(out.AcceptedParts))
	for _, part := range out.AcceptedParts {
		accepted[part.Index] = part.ServerID
	}
	partsComplete := !status.VoicePartsIncomplete && (len(status.VoiceChunks) > 0 || len(accepted) == 0)
	out.UnconfirmedParts = nil
	out.PartsIncomplete = status.VoicePartsIncomplete
	seen := map[int]bool{}
	for _, chunk := range status.VoiceChunks {
		if seen[chunk.Index] {
			partsComplete = false
			continue
		}
		seen[chunk.Index] = true
		if chunk.State != "accepted" || !chunk.HasReceipt || !ValidServerReceiptID(chunk.ServerID) {
			partsComplete = false
			if accepted[chunk.Index] == "" {
				partState := "unknown"
				switch chunk.State {
				case "prepared", "submitting", "rejected", "needs_review", "cancelled":
					partState = chunk.State
				}
				out.UnconfirmedParts = append(out.UnconfirmedParts, apphost.ReceiptPart{Index: chunk.Index, State: partState})
			}
			continue
		}
		if previous, exists := accepted[chunk.Index]; exists {
			partsComplete = partsComplete && previous == chunk.ServerID
			continue
		}
		accepted[chunk.Index] = chunk.ServerID
		out.AcceptedParts = append(out.AcceptedParts, apphost.ReceiptPart{Index: chunk.Index, ServerID: chunk.ServerID})
	}
	switch status.State {
	case "accepted":
		if identity.ValidPathID(status.ID) && ValidServerReceiptID(status.ServerID) && partsComplete {
			out.State, out.ServerID = "accepted", status.ServerID
		}
	case "prepared":
		out.State = "not_submitted"
	case "queued", "running", "sending", "submitting":
		out.State = "submitted_pending"
	case "failed", "rejected":
		out.State = "failed"
	case "cancelled":
		out.State = "cancelled"
	}
}

// ValidServerReceiptID accepts canonical nonzero platform receipt numbers.
func ValidServerReceiptID(value string) bool {
	id, err := strconv.ParseUint(value, 10, 64)
	return err == nil && id > 0 && strconv.FormatUint(id, 10) == value
}

// ProjectActionReceipt maps the platform's action state and code.
func ProjectActionReceipt(out *apphost.ActionReceipt, status linuxnative.ActionStatus) {
	out.PlatformOperationID, out.PlatformState, out.PlatformCode = status.ID, "", ""
	if validResultCode(status.State) {
		out.PlatformState = status.State
	}
	if validResultCode(status.Code) {
		out.PlatformCode = status.Code
	}
	switch status.State {
	case "succeeded", "failed", "cancelled":
		out.State = status.State
	case "rejected":
		out.State = "failed"
	case "queued", "running", "sending", "submitting":
		out.State = "submitted_pending"
	default:
		out.State = "submission_unknown"
	}
}
