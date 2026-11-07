package admin

import (
	"context"
	"errors"
	"testing"

	mobile "wechat-bot/internal/devicespec"
)

func TestRetiredDeviceControlRejectsRuntimeEntrancesBeforeDiscovery(t *testing.T) {
	f := setup(t)
	f.manager.mobileRetired = true

	if err := f.manager.mobileDeviceReady(context.Background()); !errors.Is(err, errDeviceServiceRetired) {
		t.Fatalf("device readiness = %v, want retirement error", err)
	}
	if _, err := f.manager.newMobilePluginBridge(context.Background(), mobile.Task{}, mobile.Settings{}); !errors.Is(err, errDeviceServiceRetired) {
		t.Fatalf("device bridge = %v, want retirement error", err)
	}
	if err := f.manager.mobilePlannerReady(context.Background()); !errors.Is(err, errDeviceServiceRetired) {
		t.Fatalf("planner readiness = %v, want retirement error", err)
	}
	if _, _, err := f.manager.currentMobilePlanningSource(mobile.Task{}, mobile.Observation{}); !errors.Is(err, errDeviceServiceRetired) {
		t.Fatalf("planning source = %v, want retirement error", err)
	}
}
