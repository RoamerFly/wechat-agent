// station-cutover-native is a synthetic, loopback-only native daemon used by
// the Windows old -> new -> old cutover test. It deliberately cannot read a
// real WeChat data directory or talk to an upstream service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	fixtureName    = "cutover-fixture.json"
	tokenName      = "api-token"
	fixtureToken   = "synthetic-cutover-native-token"
	fixtureAccount = "synthetic-cutover-account"
	generation     = "synthetic-cutover-generation"
)

type fixtureConfig struct {
	Dataset string `json:"dataset"`
	Account string `json:"account"`
}

type shutdownRequest struct {
	PID                 int    `json:"pid"`
	ServerID            string `json:"serverId"`
	OwnershipGeneration string `json:"ownershipGeneration"`
}

type fixture struct {
	dataDir string
	account string
	server  string
	pid     int
	http    *http.Server
	stop    sync.Once
	stopped chan error
}

func main() {
	dataDir, listen, err := parseArgs(os.Args[1:])
	if err != nil {
		fatal(err)
	}
	if err := validateLoopback(listen); err != nil {
		fatal(err)
	}
	config, err := loadConfig(dataDir)
	if err != nil {
		fatal(err)
	}
	if err := validateToken(dataDir); err != nil {
		fatal(err)
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		fatal(fmt.Errorf("listen synthetic fixture: %w", err))
	}
	defer listener.Close()

	f := &fixture{dataDir: dataDir, account: config.Account, server: config.Dataset, pid: os.Getpid(), stopped: make(chan error, 1)}
	f.http = &http.Server{Handler: http.HandlerFunc(f.serveHTTP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second}
	if err := f.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(fmt.Errorf("serve synthetic fixture: %w", err))
	}
	if err := <-f.stopped; err != nil {
		fatal(fmt.Errorf("stop synthetic fixture: %w", err))
	}
}

func parseArgs(args []string) (string, string, error) {
	flags := flag.NewFlagSet("station-cutover-native", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "")
	serverConfig := flags.String("server-config", "", "")
	dataDir := flags.String("data-dir", "", "")
	listen := flags.String("listen", "", "")
	if err := flags.Parse(args); err != nil {
		return "", "", errors.New("invalid synthetic fixture arguments")
	}
	if !*jsonOutput || flags.NArg() != 1 || flags.Arg(0) != "serve" || *serverConfig == "" || *dataDir == "" || *listen == "" {
		return "", "", errors.New("expected --json --server-config <path> --data-dir <path> --listen 127.0.0.1:<port> serve")
	}
	if !filepath.IsAbs(*serverConfig) || !filepath.IsAbs(*dataDir) {
		return "", "", errors.New("synthetic fixture requires absolute server-config and data-dir")
	}
	if info, err := os.Stat(*serverConfig); err != nil || info.IsDir() {
		return "", "", errors.New("synthetic fixture requires a prewritten server-config file")
	}
	return filepath.Clean(*dataDir), *listen, nil
}

func validateLoopback(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host == "" || port == "" {
		return errors.New("synthetic fixture requires a loopback host:port listen address")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("synthetic fixture refuses non-loopback listen addresses")
	}
	return nil
}

func loadConfig(dataDir string) (fixtureConfig, error) {
	var config fixtureConfig
	data, err := os.ReadFile(filepath.Join(dataDir, fixtureName))
	if err != nil {
		return config, errors.New("synthetic fixture requires a prewritten cutover-fixture.json")
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, errors.New("invalid synthetic cutover-fixture.json")
	}
	if (config.Dataset != "synthetic-old-dataset" && config.Dataset != "synthetic-new-dataset") || config.Account != fixtureAccount {
		return config, errors.New("synthetic fixture configuration is not an approved cutover dataset")
	}
	return config, nil
}

func validateToken(dataDir string) error {
	data, err := os.ReadFile(filepath.Join(dataDir, tokenName))
	if err != nil || strings.TrimSpace(string(data)) != fixtureToken {
		return errors.New("synthetic fixture requires its fixed api-token")
	}
	return nil
}

func (f *fixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fixtureToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "synthetic fixture authorization required"})
		return
	}
	accountPath := "/v1/accounts/" + f.account
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		writeJSON(w, http.StatusOK, map[string]any{"pid": f.pid, "dataDir": f.dataDir, "serverId": f.server, "ownershipGeneration": generation})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/accounts":
		writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{f.accountStatus()}})
	case r.Method == http.MethodGet && r.URL.Path == accountPath:
		writeJSON(w, http.StatusOK, f.accountStatus())
	case r.Method == http.MethodGet && r.URL.Path == accountPath+"/events":
		f.recordMarker("events-observed")
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "nextCursor": r.URL.Query().Get("after")})
	case r.Method == http.MethodGet && r.URL.Path == accountPath+"/sends/by-idempotency-key":
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "synthetic send checkpoint not found"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/daemon/shutdown":
		f.shutdown(w, r)
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			f.recordUnexpectedMutation()
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "synthetic fixture denies mutations"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "synthetic fixture endpoint not found"})
	}
}

func (f *fixture) accountStatus() map[string]any {
	return map[string]any{"id": f.account, "alias": f.account, "authenticated": true, "connection": "online", "storageMode": "managed"}
}

func (f *fixture) shutdown(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var request shutdownRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4097)).Decode(&request); err != nil || request.PID != f.pid || request.ServerID != f.server || request.OwnershipGeneration != generation {
		f.recordUnexpectedMutation()
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "synthetic fixture shutdown ownership mismatch"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"accepted": true})
	f.stop.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f.stopped <- f.http.Shutdown(ctx)
		}()
	})
}

func (f *fixture) recordUnexpectedMutation() {
	f.recordMarker("unexpected-native-write")
}

func (f *fixture) recordMarker(name string) {
	file, err := os.OpenFile(filepath.Join(f.dataDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, _ = file.WriteString("synthetic fixture marker\n")
		_ = file.Close()
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "station-cutover-native:", err); os.Exit(2) }
