package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/devicebroker"
	mobile "wechat-bot/internal/devicespec"
	"wechat-bot/internal/pluginruntime"
)

type observationTestIOHost struct {
	mobile.IOHost
	open func(context.Context, mobile.IOOptions) (mobile.IOBroker, error)
}

func (h observationTestIOHost) Open(ctx context.Context, options mobile.IOOptions) (mobile.IOBroker, error) {
	return h.open(ctx, options)
}

// Runs the same installed-package / devicebroker.OpenIO contract as
// TestLiveMobileDevicePluginContract, but through the host's ObserveOnce API.
// Its ADB endpoint and runner are synthetic; no device, model, or message is
// involved. Keeping this test opt-in matches the existing installed runtime
// contract because it relies on the packaged Python runtime.
func TestMobileObservationOnceInstalledRuntime(t *testing.T) {
	python := os.Getenv("WECHAT_PLUGIN_TEST_PYTHON")
	if python == "" {
		t.Skip("opt-in installed mobile runtime replay")
	}
	python, _ = filepath.Abs(python)
	f := setup(t)
	service := f.manager.mobile.(*mobileWorkflowService)
	settings := service.Settings()
	settings.Enabled = true
	settings.Devices = []string{"synthetic"}
	settings.Actions = []string{"capture"}
	options, settings := mobileDeviceIOResourceFixture(t, f.manager, settings, python)
	helper, _ := filepath.Abs("../../scripts/windows_plugin_host.pyw")
	options.WindowsHelper = helper
	rawOptions, _ := json.Marshal(options)
	t.Setenv("WECHAT_PLUGIN_RUNTIME_CONFIG", string(rawOptions))
	if err := service.Save(settings); err != nil {
		t.Fatal(err)
	}
	store, err := f.manager.packageStore()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.Prepare(t.Context(), "../../plugins/mobile-control")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Install(t.Context(), prepared.Token, prepared.Digest, prepared.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetEnabled(t.Context(), installed.Package.ID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	fishID, err := f.manager.fish.Bind(f.manager.native.Account())
	if err != nil {
		t.Fatal(err)
	}
	boundFishID, bindingID, err := f.manager.fish.BindDevice(f.manager.native.Account(), "synthetic")
	if err != nil || boundFishID != fishID || bindingID == "" {
		t.Fatalf("bind fish device fish=%q binding=%q err=%v", boundFishID, bindingID, err)
	}
	controller, err := pluginruntime.NewController(store, pluginruntime.InstalledExecutor(store, options))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	f.manager.packageRuntime, f.manager.packageRuntimeReady = controller, true
	var pngData bytes.Buffer
	if err = png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 16, 12))); err != nil {
		t.Fatal(err)
	}
	server, calls := &mobileIOTestADBServer{}, 0
	f.manager.deviceIO = observationTestIOHost{IOHost: devicebroker.Host{}, open: func(ctx context.Context, input mobile.IOOptions) (mobile.IOBroker, error) {
		option := devicebroker.Options{IOOptions: input, ADBServer: server}
		option.Runner = func(ctx context.Context, path string, args []string, limit int64, timeout time.Duration) ([]byte, error) {
			calls++
			if path != filepath.Join(options.DeviceToolsRoots[0], "adb.exe") || len(args) < 7 || !reflect.DeepEqual(args[:6], []string{"-H", "127.0.0.1", "-P", "5037", "-s", "synthetic-serial-synthetic"}) || limit < 1 || timeout <= 0 {
				return nil, errors.New("unexpected synthetic adb invocation")
			}
			tail := args[6:]
			switch {
			case reflect.DeepEqual(tail, []string{"get-state"}):
				return []byte("device\n"), nil
			case reflect.DeepEqual(tail, []string{"shell", "dumpsys", "window"}):
				return []byte("mCurrentFocus=Window{synthetic com.example.home/.Main}"), nil
			case reflect.DeepEqual(tail, []string{"exec-out", "screencap", "-p"}):
				return pngData.Bytes(), nil
			case len(tail) == 4 && tail[0] == "shell" && tail[1] == "uiautomator" && tail[2] == "dump" && strings.HasPrefix(tail[3], "/sdcard/whale-ui-"):
				return nil, nil
			case len(tail) == 3 && tail[0] == "exec-out" && tail[1] == "cat" && strings.HasSuffix(tail[2], ".xml"):
				return []byte(`<hierarchy><node text="synthetic page" package="com.example.home" bounds="[0,0][16,12]"/></hierarchy>`), nil
			case len(tail) == 4 && tail[0] == "shell" && tail[1] == "rm" && tail[2] == "-f":
				return nil, nil
			default:
				return nil, errors.New("unexpected synthetic adb operation")
			}
		}
		return devicebroker.OpenIO(ctx, option)
	}}
	started, err := service.Start(mobile.Source{Admin: true, MessageID: "fish-bound-start"}, "synthetic", "查看授权设备")
	if err != nil || started.FishID != fishID || started.DeviceBindingID != bindingID {
		t.Fatalf("fish-bound start task=%#v err=%v", started, err)
	}
	if err = service.StopAll(); err != nil {
		t.Fatal(err)
	}
	view, err := service.ObserveOnce(t.Context(), "synthetic")
	if err != nil {
		jobs, _ := store.Jobs(t.Context(), mobileDevicePluginID, 0, 20)
		raw, _ := json.Marshal(jobs.Items)
		t.Fatalf("observe=%v jobs=%s", err, raw)
	}
	if calls == 0 || server.acquisitions == 0 || view.ObservedAt.IsZero() || view.CurrentApp != "com.example.home" || view.TaskID == "" || view.ScreenshotURI == "" {
		t.Fatalf("view=%#v calls=%d leases=%d", view, calls, server.acquisitions)
	}
	if observed, ok := service.Get(view.TaskID); !ok || observed.FishID != fishID || observed.DeviceBindingID != bindingID {
		t.Fatalf("observation task did not retain fish binding task=%#v found=%t", observed, ok)
	}
	data, err := service.Screenshot(view.TaskID, view.ObservationID)
	if err != nil || !bytes.Equal(data, pngData.Bytes()) {
		t.Fatalf("screenshot not retained err=%v", err)
	}
	f.login()
	if status, body, response := f.request("GET", view.ScreenshotURI, nil, false, false); status != 200 || response.Header.Get("Cache-Control") != "no-store" || !bytes.Equal(body, pngData.Bytes()) {
		t.Fatalf("protected screenshot route status=%d cache=%q bytes=%d", status, response.Header.Get("Cache-Control"), len(body))
	}
	before := calls
	if _, err = service.SetObservationPaused("synthetic", true, true); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ObserveOnce(t.Context(), "synthetic"); err == nil || err.Error() != "device_observation_paused" {
		t.Fatalf("paused observe err=%v", err)
	}
	if calls != before {
		t.Fatalf("paused state reached the device again before=%d after=%d", before, calls)
	}
}
