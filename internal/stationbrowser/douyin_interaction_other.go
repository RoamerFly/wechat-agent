//go:build !windows

package stationbrowser

import "context"

func RunDouyinInteraction(context.Context, string, DouyinInteractionRequest, func() error) (DouyinInteractionResult, error) {
	return DouyinInteractionResult{State: DouyinInteractionUnknown}, ErrDouyinInteractionUnavailable
}
