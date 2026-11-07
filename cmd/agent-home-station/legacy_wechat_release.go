package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"

	"wechat-bot/internal/station"
)

type legacyWechatReleaseArgs struct {
	dataDir       string
	connectionID  string
	auditDigest   string
	acceptUnknown bool
	applyDigest   string
}

func parseLegacyWechatReleaseArgs(args []string) (legacyWechatReleaseArgs, error) {
	flags := flag.NewFlagSet("legacy-wechat-release", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data", "", "成长站私有 data 绝对目录")
	connectionID := flags.String("connection", "", "需要释放的微信连接标识")
	auditDigest := flags.String("audit-digest", "", "已核对的 SHA-256 审计摘要")
	acceptUnknown := flags.Bool("accept-unknown", false, "接受审计中不能自动判定的项目")
	applyDigest := flags.String("apply", "", "确认预演返回的令牌后执行释放")
	if err := flags.Parse(args); err != nil {
		return legacyWechatReleaseArgs{}, err
	}
	if flags.NArg() != 0 || *dataDir == "" || !filepath.IsAbs(*dataDir) || *connectionID == "" {
		return legacyWechatReleaseArgs{}, errors.New("微信旧连接释放需要明确指定 data 绝对目录和 connection")
	}
	if digest, err := hex.DecodeString(*auditDigest); err != nil || len(digest) != 32 {
		return legacyWechatReleaseArgs{}, errors.New("请提供完整的 SHA-256 审计摘要")
	}
	return legacyWechatReleaseArgs{
		dataDir: *dataDir, connectionID: *connectionID, auditDigest: *auditDigest,
		acceptUnknown: *acceptUnknown, applyDigest: *applyDigest,
	}, nil
}

// runLegacyWechatRelease is deliberately a standalone command: it does not
// construct an HTTP handler, connect a native daemon, or start an application
// service. Without --apply it only asks station for a preflight preview.
func runLegacyWechatRelease(ctx context.Context, args []string, output io.Writer) error {
	request, err := parseLegacyWechatReleaseArgs(args)
	if err != nil {
		return err
	}
	result, err := station.ReleaseLegacyWechat(ctx, station.LegacyWechatReleaseRequest{
		DataDir: request.dataDir, ConnectionID: request.connectionID, AuditDigest: request.auditDigest,
		AcceptUnknown: request.acceptUnknown, ApplyDigest: request.applyDigest,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
