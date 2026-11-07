package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/workspacefs"
)

// StationRegistration admits only the daemon launched for the station's
// private WeChat directory. Account setup is owner-only; Agent workers are separate.
func StationRegistration(ctx context.Context, endpoint string, pid int, stationData string) (apphost.Registration, error) {
	nativeDir := filepath.Join(stationData, "apps", ID, "native")
	root, err := workspacefs.OpenPath(nativeDir)
	if err != nil {
		return apphost.Registration{}, errors.New("微信原生私有目录不可读")
	}
	defer root.Close()
	info, err := root.Lstat("api-token")
	if err != nil || !info.Mode().IsRegular() {
		return apphost.Registration{}, errors.New("微信原生凭据文件不可读")
	}
	client, err := linuxnative.NewAdmin(linuxnative.Config{URL: endpoint, TokenFile: filepath.Join(nativeDir, "api-token"), Account: "station-probe"})
	if err != nil {
		return apphost.Registration{}, errors.New("微信原生连接配置不可用")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := client.DaemonStatus(ctx)
	if err != nil || status.PID != pid || !sameDaemonDirectory(status.DataDirectory, nativeDir) {
		return apphost.Registration{}, errors.New("微信原生实例归属核验失败")
	}
	return apphost.Registration{
		Application: apphost.Application{ID: ID, Name: "微信", InstallationID: "wechat-station-native", Registration: "station-native"},
		Provider:    &stationNativeProvider{client: client, status: status},
	}, nil
}

func sameDaemonDirectory(actual, expected string) bool {
	if runtime.GOOS == "windows" {
		comparable := func(path string) string {
			if strings.HasPrefix(path, `\\?\UNC\`) {
				path = `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
			} else {
				path = strings.TrimPrefix(path, `\\?\`)
			}
			return filepath.Clean(path)
		}
		return strings.EqualFold(comparable(actual), comparable(expected))
	}
	return filepath.Clean(actual) == filepath.Clean(expected)
}

type stationNativeProvider struct {
	client *linuxnative.Sender
	status linuxnative.DaemonStatus
}

func (p *stationNativeProvider) verify(ctx context.Context) error {
	current, err := p.client.DaemonStatus(ctx)
	if err != nil || current.PID != p.status.PID || current.ServerID != p.status.ServerID ||
		current.OwnershipGeneration != p.status.OwnershipGeneration || !sameDaemonDirectory(current.DataDirectory, p.status.DataDirectory) {
		return apphost.ErrUnavailable
	}
	return nil
}

func stationConnection(account linuxnative.AccountInfo) apphost.Connection {
	return apphost.Connection{ID: ConnectionID(account.ID), AccountID: account.ID, Label: account.Alias, Configuration: "native-only"}
}

func (p *stationNativeProvider) accounts(ctx context.Context) ([]linuxnative.AccountInfo, error) {
	if err := p.verify(ctx); err != nil {
		return nil, err
	}
	items, err := p.client.Accounts(ctx)
	if err != nil {
		return nil, apphost.ErrUnavailable
	}
	if err := p.verify(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (p *stationNativeProvider) Connections(ctx context.Context) ([]apphost.Connection, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	items, err := p.accounts(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]apphost.Connection, 0, len(items))
	for _, account := range items {
		result = append(result, stationConnection(account))
	}
	return result, nil
}

func (p *stationNativeProvider) Inspect(ctx context.Context, id string) (apphost.ConnectionStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	items, err := p.accounts(ctx)
	if err != nil {
		return apphost.ConnectionStatus{}, err
	}
	for _, account := range items {
		connection := stationConnection(account)
		if connection.ID != id {
			continue
		}
		login, connectionState := "signed_out", "unknown"
		if account.Authenticated {
			login = "authenticated"
		}
		switch account.Connection {
		case "online", "connected":
			connectionState = "online"
		case "offline", "disconnected":
			connectionState = "offline"
		case "starting", "connecting", "reconnecting":
			connectionState = "starting"
		}
		return apphost.ConnectionStatus{
			Connection: connection, Login: login, Runtime: connectionState, Worker: "unavailable",
			CapabilityCheck: "not_checked", SupportedActions: []string{},
		}, nil
	}
	return apphost.ConnectionStatus{}, apphost.ErrNotFound
}
