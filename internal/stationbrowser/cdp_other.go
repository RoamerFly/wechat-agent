//go:build !windows

package stationbrowser

import "context"

func Available() bool                                      { return false }
func openCDP(context.Context, string) (*cdpSession, error) { return nil, errCDPUnavailable }
func openRestrictedCDP(context.Context, string, func(string) bool) (*cdpSession, error) {
	return nil, errCDPUnavailable
}
