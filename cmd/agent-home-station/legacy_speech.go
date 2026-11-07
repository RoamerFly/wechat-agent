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

func runLegacySpeech(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("legacy-speech", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dataDir := flags.String("data", "", "成长站私有数据绝对目录")
	workspace := flags.String("workspace", "", "工作空间根目录，默认使用 data/managed-workspaces")
	planFile := flags.String("plan", "", "主人私有声线接续计划 JSON 的绝对路径")
	digest := flags.String("digest", "", "确认原预演摘要，未填写时仅预演")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || !filepath.IsAbs(*dataDir) || !filepath.IsAbs(*planFile) {
		return errors.New("声线接续需要明确指定 data 和 plan 的绝对路径")
	}
	if *workspace == "" {
		*workspace = filepath.Join(*dataDir, "managed-workspaces")
	}
	if !filepath.IsAbs(*workspace) {
		return errors.New("工作空间根目录需要绝对路径")
	}
	var plan station.LegacySpeechPlan
	var err error
	status := "preview"
	if *digest == "" {
		plan, err = station.PreviewLegacySpeech(ctx, *dataDir, *workspace, *planFile)
	} else {
		if value, err := hex.DecodeString(*digest); err != nil || len(value) != 32 {
			return errors.New("请使用声线接续预演返回的完整摘要")
		}
		plan, err = station.ImportLegacySpeech(ctx, *dataDir, *workspace, *planFile, *digest)
		status = "applied"
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		Status string `json:"status"`
		station.LegacySpeechPlan
	}{Status: status, LegacySpeechPlan: plan})
}
