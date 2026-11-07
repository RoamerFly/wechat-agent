package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"wechat-bot/internal/admin"
	"wechat-bot/internal/linuxnative"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "desktop-clean-accounts" {
		if err := admin.FinishNativeAccountPurges(os.Args[2], os.Args[3]); err != nil {
			log.Print("未能清理已删除账号的原生数据：", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) == 3 && os.Args[1] == "desktop-busy" {
		if err := printDesktopBusy(os.Args[2]); err != nil {
			log.Print("wechat-admin: cannot verify native pending jobs")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		host, port, err := net.SplitHostPort(env("WECHAT_ADMIN_LISTEN", "127.0.0.1:8080"))
		if err != nil {
			os.Exit(1)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal("wechat-admin: startup or runtime failed; check private configuration and port availability")
	}
}
func run() error {
	dir := os.Getenv("WECHAT_ADMIN_DATA_DIR")
	if dir == "" {
		dir = "/data/admin"
	}
	password, err := admin.Password(dir, os.Getenv("WECHAT_ADMIN_PASSWORD"))
	if err != nil {
		return err
	}
	native, err := linuxnative.NewAdmin(linuxnative.Config{URL: env("WECHAT_NATIVE_URL", "http://127.0.0.1:6174"), TokenFile: env("WECHAT_NATIVE_TOKEN_FILE", "/data/api-token"), Account: env("WECHAT_NATIVE_ACCOUNT", "background-test")})
	if err != nil {
		return err
	}
	manager, err := newManager(dir, os.Getenv("WECHAT_BOT_STATE_DIR"), native)
	if err != nil {
		return err
	}
	defer manager.Close()
	handler, err := admin.NewHandler(manager, password, os.Getenv("WECHAT_ADMIN_ORIGIN"))
	if err != nil {
		return err
	}
	listen := env("WECHAT_ADMIN_LISTEN", "127.0.0.1:8080")
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 18 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("wechat-admin listening on %s; bootstrap credential: %s", listen, filepath.Join(dir, "admin-token"))
	stopped := make(chan error, 1)
	go func() { stopped <- server.ListenAndServe() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var desktop <-chan struct{}
	if len(os.Args) == 2 && os.Args[1] == "--desktop" {
		desktop = desktopStop(os.Stdin)
	}
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
	case <-desktop:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
