//go:build windows

package apphost

import (
	"context"

	"wechat-bot/internal/obspreview"
)

// NewWindowsOBSOutput uses the Windows job-owned encoder. Provision must
// resolve only a host-approved runtime with a dedicated, loopback-only control
// configuration. A platform adapter and provisioner are configured separately.
func NewWindowsOBSOutput(provision func(context.Context, LiveReference) (obspreview.EncoderLaunch, error)) *ManagedLiveOutput {
	if provision == nil {
		return nil
	}
	return NewManagedLiveOutput(func(ctx context.Context, ref LiveReference) (LiveEncoder, error) {
		config, err := provision(ctx, ref)
		if err != nil {
			return nil, err
		}
		return obspreview.LaunchEncoder(ctx, config)
	})
}
