package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"

	"wechat-bot/internal/station"
)

// legacyWechatSnapshotArgs deliberately keeps capture and verification input
// separate.  Capture reads only an explicitly named legacy directory; verify
// reads only the previously written snapshot.  Neither mode starts the web
// service or contacts a native WeChat process.
type legacyWechatSnapshotArgs struct {
	legacyDataDir string
	snapshotDir   string
	accountID     string
	applyDigest   string
	verify        bool
	audit         bool
	checkSource   bool
}

func parseLegacyWechatSnapshotArgs(args []string) (legacyWechatSnapshotArgs, error) {
	flags := flag.NewFlagSet("legacy-wechat-snapshot", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	legacyDataDir := flags.String("legacy-data", "", "旧微信数据绝对目录")
	snapshotDir := flags.String("snapshot", "", "快照绝对目录")
	accountID := flags.String("account", "", "快照所属的主账号标识")
	applyDigest := flags.String("apply", "", "确认预演返回的摘要后写入快照")
	verify := flags.Bool("verify", false, "只校验已有快照")
	audit := flags.Bool("audit", false, "只读审计旧微信待交接队列")
	checkSource := flags.Bool("check-source", false, "只读核对旧资料与快照目标，无需停机凭证")
	if err := flags.Parse(args); err != nil {
		return legacyWechatSnapshotArgs{}, err
	}
	if flags.NArg() != 0 {
		return legacyWechatSnapshotArgs{}, errors.New("微信交接快照不接受位置参数")
	}
	if *audit {
		if *snapshotDir != "" || *applyDigest != "" || *verify || *checkSource || *legacyDataDir == "" || !filepath.IsAbs(*legacyDataDir) || *accountID == "" {
			return legacyWechatSnapshotArgs{}, errors.New("审计旧微信队列时仅使用 --audit --legacy-data <绝对目录> --account <账号>")
		}
		return legacyWechatSnapshotArgs{legacyDataDir: *legacyDataDir, accountID: *accountID, audit: true}, nil
	}
	if *snapshotDir == "" || !filepath.IsAbs(*snapshotDir) {
		return legacyWechatSnapshotArgs{}, errors.New("微信交接快照需要明确指定 snapshot 绝对目录")
	}

	if *verify {
		if *legacyDataDir != "" || *accountID != "" || *applyDigest != "" || *checkSource {
			return legacyWechatSnapshotArgs{}, errors.New("校验已有快照时仅使用 --snapshot <绝对目录> --verify")
		}
		return legacyWechatSnapshotArgs{snapshotDir: *snapshotDir, verify: true}, nil
	}

	if *legacyDataDir == "" || !filepath.IsAbs(*legacyDataDir) || *accountID == "" {
		return legacyWechatSnapshotArgs{}, errors.New("创建微信交接快照需要明确指定 legacy-data 绝对目录、snapshot 绝对目录和 account")
	}
	if *checkSource && *applyDigest != "" {
		return legacyWechatSnapshotArgs{}, errors.New("只读来源核对不能使用 --apply")
	}
	return legacyWechatSnapshotArgs{
		legacyDataDir: *legacyDataDir,
		snapshotDir:   *snapshotDir,
		accountID:     *accountID,
		applyDigest:   *applyDigest,
		checkSource:   *checkSource,
	}, nil
}

// runLegacyWechatSnapshot is an offline handover command.  The station layer
// returns a deliberately safe result: manifest metadata, verification status
// and a confirmation digest, never account credentials or message bodies.
func runLegacyWechatSnapshot(ctx context.Context, args []string, output io.Writer) error {
	request, err := parseLegacyWechatSnapshotArgs(args)
	if err != nil {
		return err
	}
	if request.audit {
		result, err := station.AuditLegacyWechatQueue(ctx, request.legacyDataDir, request.accountID)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	result, err := station.CaptureLegacyWechatSnapshot(ctx, station.LegacyWechatSnapshotRequest{
		LegacyDataDir:  request.legacyDataDir,
		SnapshotDir:    request.snapshotDir,
		PrimaryAccount: request.accountID,
		ApplyDigest:    request.applyDigest,
		Verify:         request.verify,
		CheckSource:    request.checkSource,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
