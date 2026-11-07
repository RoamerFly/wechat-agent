// Command agent-home-station starts the local, application-independent home
// station for 大肥鱼. No WeChat account, native bridge or model service is
// required. A desktop-owned native service can be admitted explicitly.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/station"
	"wechat-bot/internal/stationauth"
	"wechat-bot/internal/stationruntime"
)

const defaultListen = "127.0.0.1:8082"

type config struct {
	dataDir              string
	workspaceRoot        string
	listen               string
	runtimeFile          string
	desktop              bool
	wechatNativeURL      string
	wechatNativePID      int
	legacySettingsSource string
	legacySettingsDigest string
	legacyHomeSource     string
	legacyHomeTarget     string
	legacyHomeDigest     string
	legacyHomeMaxBytes   int64
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		// Errors returned here describe local operations, without provider
		// responses or credential values.
		log.Printf("Agent-home-station 执行失败：%v", err)
		os.Exit(1)
	}
}

// run is deliberately independent from the working directory. It is kept
// separate from main so desktop integration can stop it using a context and
// tests can validate the command without signals.
func run(ctx context.Context, args []string, output io.Writer) error {
	return runWithDesktopInput(ctx, args, output, os.Stdin)
}

// runWithDesktopInput keeps the desktop pipe injectable for lifecycle tests.
// It deliberately ignores input unless --desktop was explicitly selected;
// ordinary CLI callers can therefore inherit a closed stdin without exiting.
func runWithDesktopInput(ctx context.Context, args []string, output io.Writer, input io.Reader) error {
	if len(args) > 0 && args[0] == "legacy-speech" {
		return runLegacySpeech(ctx, args[1:], output)
	}
	if len(args) > 0 && args[0] == "legacy-rvc" {
		return runLegacyRVC(ctx, args[1:], output)
	}
	if len(args) > 0 && args[0] == "legacy-wechat-release" {
		return runLegacyWechatRelease(ctx, args[1:], output)
	}
	if len(args) > 0 && args[0] == "legacy-wechat-snapshot" {
		return runLegacyWechatSnapshot(ctx, args[1:], output)
	}
	path, err := os.Executable()
	if err != nil {
		return errors.New("cannot determine executable location")
	}
	cfg, err := parseConfig(args, path)
	if err != nil {
		return err
	}
	if cfg.legacyHomeSource != "" {
		var plan station.LegacyHomePlan
		status := "preview"
		if cfg.legacyHomeDigest == "" {
			plan, err = station.PreviewLegacyHome(ctx, cfg.dataDir, cfg.workspaceRoot, cfg.legacyHomeSource, cfg.legacyHomeTarget, cfg.legacyHomeMaxBytes)
		} else {
			plan, err = station.ImportLegacyHome(ctx, cfg.dataDir, cfg.workspaceRoot, cfg.legacyHomeSource, cfg.legacyHomeTarget, cfg.legacyHomeMaxBytes, cfg.legacyHomeDigest)
			status = "applied"
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(struct {
			Status string `json:"status"`
			station.LegacyHomePlan
		}{Status: status, LegacyHomePlan: plan})
	}
	if cfg.legacySettingsSource != "" {
		if cfg.legacySettingsDigest == "" {
			plan, err := station.PreviewLegacySettings(ctx, cfg.legacySettingsSource, cfg.dataDir)
			if err != nil {
				return err
			}
			return json.NewEncoder(output).Encode(struct {
				Status string `json:"status"`
				station.LegacySettingsPlan
			}{Status: "preview", LegacySettingsPlan: plan})
		}
		plan, err := station.ImportLegacySettings(ctx, cfg.legacySettingsSource, cfg.dataDir, cfg.legacySettingsDigest)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(struct {
			Status string `json:"status"`
			station.LegacySettingsPlan
		}{Status: "applied", LegacySettingsPlan: plan})
	}

	password, err := stationauth.Password(cfg.dataDir, os.Getenv("AHS_ADMIN_PASSWORD"))
	if err != nil {
		return err
	}
	windows, err := stationruntime.LoadWindowsRunner(filepath.Dir(path), cfg.dataDir, cfg.workspaceRoot, cfg.runtimeFile)
	if err != nil {
		return err
	}
	var registry apphost.Registry
	if cfg.wechatNativeURL != "" {
		registration, attachErr := wechatapp.StationRegistration(ctx, cfg.wechatNativeURL, cfg.wechatNativePID, cfg.dataDir)
		if attachErr != nil {
			return attachErr
		}
		registry, err = apphost.New(registration)
		if err != nil {
			return err
		}
	}
	handler, err := station.NewLocalHomeHandlerWithApps(cfg.dataDir, cfg.workspaceRoot, password, "", registry, windows)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = handler.CloseContext(closeCtx)
	}()

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	fmt.Fprintf(output, "Agent Home Station 已启动：%s\n工作空间：%s\n", listener.Addr().String(), cfg.workspaceRoot)
	var desktop <-chan struct{}
	if cfg.desktop {
		desktop = desktopStop(input)
	}
	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	case <-desktop:
	}

	// Cancel model/tool work before HTTP graceful shutdown. Otherwise a request
	// that is already running can consume the entire HTTP timeout while the
	// desktop waits to exit.
	handler.BeginShutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	closeErr := handler.CloseContext(closeCtx)
	closeCancel()
	if shutdownErr != nil {
		return errors.Join(shutdownErr, closeErr)
	}
	if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.Join(err, closeErr)
	}
	return closeErr
}

func parseConfig(args []string, executable string) (config, error) {
	if executable == "" {
		return config{}, errors.New("executable path is unavailable")
	}
	fs := flag.NewFlagSet("agent-home-station", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dataFlag := fs.String("data", "", "本机身份与设置目录（绝对路径）")
	workspaceFlag := fs.String("workspace", "", "大肥鱼工作空间根目录（绝对路径）")
	listenFlag := fs.String("listen", defaultListen, "仅本机监听地址")
	runtimeFlag := fs.String("runtime-config", "", "可选 Windows 运行器配置文件绝对路径")
	desktopFlag := fs.Bool("desktop", false, "由桌面程序持有的私有停止管道模式")
	wechatNativeURL := fs.String("wechat-native-url", "", "桌面持有的微信原生服务地址")
	wechatNativePID := fs.Int("wechat-native-pid", 0, "桌面持有的微信原生服务进程编号")
	legacySettingsSource := fs.String("legacy-settings-source", "", "预演接续旧宿主设置的 admin 绝对目录，完成后退出")
	legacySettingsDigest := fs.String("legacy-settings-digest", "", "按预演摘要确认接续到全新 data 目录，完成后退出")
	legacyHomeSource := fs.String("legacy-home-source", "", "预演接续选定的旧项目或笔记绝对目录，完成后退出")
	legacyHomeTarget := fs.String("legacy-home-target", "", "接续到鱼家目录内尚不存在的相对目录")
	legacyHomeDigest := fs.String("legacy-home-digest", "", "按预演摘要确认接续选定资料，完成后退出")
	legacyHomeMaxBytes := fs.Int64("legacy-home-max-bytes", 1<<30, "本次资料接续的可配置字节预算")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() != 0 {
		return config{}, errors.New("unexpected positional arguments")
	}
	if *legacyHomeSource != "" {
		if !filepath.IsAbs(*legacyHomeSource) || *dataFlag == "" || *legacyHomeTarget == "" || *legacyHomeMaxBytes <= 0 {
			return config{}, errors.New("资料接续需要明确的 data、旧资料绝对目录、家目录内目标及正数字节预算")
		}
		if *desktopFlag || *wechatNativeURL != "" || *wechatNativePID != 0 || *runtimeFlag != "" || *legacySettingsSource != "" || *legacySettingsDigest != "" {
			return config{}, errors.New("资料接续需要单独运行，不能同时启动服务或接续模型设置")
		}
	} else if *legacyHomeTarget != "" || *legacyHomeDigest != "" {
		return config{}, errors.New("请同时指定资料接续来源及家目录内目标")
	}
	if *legacyHomeDigest != "" {
		if digest, err := hex.DecodeString(*legacyHomeDigest); err != nil || len(digest) != 32 {
			return config{}, errors.New("请提供资料接续预演返回的完整摘要")
		}
	}
	if *legacySettingsSource != "" {
		if !filepath.IsAbs(*legacySettingsSource) || *dataFlag == "" {
			return config{}, errors.New("旧设置接续需要旧 admin 绝对目录和明确指定的 data 目录")
		}
		if *desktopFlag || *wechatNativeURL != "" || *wechatNativePID != 0 || *runtimeFlag != "" {
			return config{}, errors.New("旧设置接续需要单独运行，不能同时启动桌面或应用服务")
		}
	}
	if *legacySettingsDigest != "" {
		if digest, err := hex.DecodeString(*legacySettingsDigest); err != nil || len(digest) != 32 || *legacySettingsSource == "" {
			return config{}, errors.New("请提供旧设置预演返回的完整摘要及来源目录")
		}
	}

	executable, err := filepath.Abs(executable)
	if err != nil {
		return config{}, err
	}
	dataDir := *dataFlag
	if dataDir == "" {
		dataDir = filepath.Join(filepath.Dir(executable), "data", "station")
	} else if !filepath.IsAbs(dataDir) {
		return config{}, errors.New("data directory must be absolute")
	}
	dataDir = filepath.Clean(dataDir)

	workspaceRoot := *workspaceFlag
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(dataDir, "managed-workspaces")
	} else if !filepath.IsAbs(workspaceRoot) {
		return config{}, errors.New("workspace directory must be absolute")
	}
	workspaceRoot = filepath.Clean(workspaceRoot)

	listen, err := localListenAddress(*listenFlag)
	if err != nil {
		return config{}, err
	}
	if *runtimeFlag != "" && !filepath.IsAbs(*runtimeFlag) {
		return config{}, errors.New("runtime configuration must be absolute")
	}
	if *wechatNativeURL != "" || *wechatNativePID != 0 {
		endpoint, err := url.Parse(*wechatNativeURL)
		if err != nil || !*desktopFlag || *wechatNativePID <= 0 || endpoint.Scheme != "http" ||
			endpoint.Hostname() != "127.0.0.1" || endpoint.User != nil || endpoint.Path != "" ||
			endpoint.ForceQuery || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return config{}, errors.New("微信原生服务要求桌面持有的本机地址和进程编号")
		}
		port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
		_, adminPort, _ := net.SplitHostPort(listen)
		if err != nil || port == 0 || strconv.FormatUint(port, 10) == adminPort {
			return config{}, errors.New("微信原生服务端口不可用或与管理端口相同")
		}
		appRoot := filepath.Join(dataDir, "apps", wechatapp.ID)
		if pathsOverlap(appRoot, workspaceRoot) {
			return config{}, errors.New("微信应用私有目录不能与工作空间重叠")
		}
	}
	return config{
		dataDir: dataDir, workspaceRoot: workspaceRoot, listen: listen, runtimeFile: *runtimeFlag, desktop: *desktopFlag,
		wechatNativeURL: *wechatNativeURL, wechatNativePID: *wechatNativePID,
		legacySettingsSource: *legacySettingsSource, legacySettingsDigest: *legacySettingsDigest,
		legacyHomeSource: *legacyHomeSource, legacyHomeTarget: *legacyHomeTarget,
		legacyHomeDigest: *legacyHomeDigest, legacyHomeMaxBytes: *legacyHomeMaxBytes,
	}, nil
}

func pathsOverlap(first, second string) bool {
	within := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	return within(first, second) || within(second, first)
}

// localListenAddress accepts only unambiguous loopback addresses. In
// particular, a host name other than localhost cannot silently resolve to a
// network interface later.
func localListenAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return "", errors.New("listen address must include a loopback host and port")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", errors.New("listen port is invalid")
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("listen address must be loopback only")
		}
	}
	// Preserve localhost for its intended local-only behaviour and normalize IP
	// spellings so callers can safely show the effective endpoint.
	if host == "localhost" {
		return net.JoinHostPort(host, strconv.FormatUint(portNumber, 10)), nil
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.FormatUint(portNumber, 10)), nil
}
