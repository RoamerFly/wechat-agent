package station

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/packagecheckout"
	"wechat-bot/internal/workspacefs"
)

const legacyHomeManifest = ".ahs-home-import.json"

// LegacyHomePlan exposes inventory totals and identity without file contents.
type LegacyHomePlan struct {
	Digest   string `json:"digest"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	FishID   string `json:"fish_id"`
	OwnerID  string `json:"owner_id"`
	Revision uint64 `json:"revision"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	MaxBytes int64  `json:"max_bytes"`
}

type legacyHomeFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type legacyHomeInventory struct {
	Files []legacyHomeFile `json:"files"`
	Dirs  []string         `json:"directories"`
}

type legacyHomeReceipt struct {
	Version int `json:"version"`
	LegacyHomePlan
	Inventory legacyHomeInventory `json:"inventory"`
}

var errLegacyHomeChanged = errors.New("资料、目标或家目录授权已变化，请重新预演")

func PreviewLegacyHome(ctx context.Context, dataDir, workspaceRoot, sourceDir, targetRelative string, maxBytes int64) (LegacyHomePlan, error) {
	receipt, err := loadLegacyHome(ctx, dataDir, workspaceRoot, sourceDir, targetRelative, maxBytes)
	return receipt.LegacyHomePlan, err
}

func loadLegacyHome(ctx context.Context, dataDir, workspaceRoot, sourceDir, targetRelative string, maxBytes int64) (legacyHomeReceipt, error) {
	if err := ctx.Err(); err != nil {
		return legacyHomeReceipt{}, err
	}
	targetRelative = filepath.ToSlash(targetRelative)
	if maxBytes <= 0 || maxBytes == math.MaxInt64 || !packagecheckout.SafePath(targetRelative) || targetRelative == "." {
		return legacyHomeReceipt{}, errors.New("请提供有效的家目录内目标和可用字节预算")
	}
	first, rest, ok := strings.Cut(targetRelative, "/")
	if !ok || rest == "" || !slices.Contains([]string{"projects", "works", "diary", "scratch", "Project Tools"}, first) {
		return legacyHomeReceipt{}, errors.New("请选择 projects、works、diary、scratch 或 Project Tools 下的新子目录")
	}
	for _, candidate := range []string{dataDir, workspaceRoot, sourceDir} {
		if !filepath.IsAbs(candidate) {
			return legacyHomeReceipt{}, errors.New("接续目录需要绝对路径")
		}
	}
	dataDir, workspaceRoot, sourceDir = filepath.Clean(dataDir), filepath.Clean(workspaceRoot), filepath.Clean(sourceDir)
	private, err := workspacefs.OpenPath(dataDir)
	if err != nil {
		return legacyHomeReceipt{}, errors.New("成长站私有目录不可用")
	}
	defer private.Close()
	file, err := workspacefs.OpenRegular(private, "fish-identity.json")
	if err != nil {
		return legacyHomeReceipt{}, errors.New("请先初始化本机主人和鱼身份")
	}
	file.Close()
	record, err := fishidentity.New(dataDir).Read()
	if err != nil || record.LocalOwner == nil || !record.LocalOwner.HomeAccess {
		return legacyHomeReceipt{}, errors.New("请先为本机主人开启家目录权限")
	}
	home, err := (&localHomeService{root: workspaceRoot, dataDir: dataDir}).directory(record)
	if err != nil {
		return legacyHomeReceipt{}, err
	}
	if pathsOverlap(sourceDir, dataDir) || pathsOverlap(sourceDir, home) {
		return legacyHomeReceipt{}, errors.New("旧资料目录不能包含宿主私有资料或与新家目录重叠")
	}
	source, err := workspacefs.OpenPath(sourceDir)
	if err != nil {
		return legacyHomeReceipt{}, errors.New("旧资料目录不可用或经过目录链接")
	}
	defer source.Close()
	if err := validateLegacyHomeSource(source, sourceDir, dataDir, home); err != nil {
		return legacyHomeReceipt{}, err
	}
	inventory, total, err := inventoryLegacyHome(ctx, source, maxBytes, false)
	if err != nil {
		return legacyHomeReceipt{}, err
	}
	plan := LegacyHomePlan{
		Source: sourceDir, Target: filepath.Join(home, filepath.FromSlash(targetRelative)),
		FishID: record.FishID, OwnerID: record.LocalOwner.ID, Revision: record.LocalOwner.Revision,
		Files: len(inventory.Files), Bytes: total, MaxBytes: maxBytes,
	}
	receipt := legacyHomeReceipt{Version: 1, LegacyHomePlan: plan, Inventory: inventory}
	raw, _ := json.Marshal(receipt)
	if len(raw) > 32<<20 {
		return legacyHomeReceipt{}, errors.New("接续清单超过 32 MiB，请拆选项目子目录")
	}
	receipt.Digest = legacySettingsHash(raw)
	return receipt, nil
}

func validateLegacyHomeSource(root *os.Root, source, dataDir, home string) error {
	if runtime.GOOS == "windows" {
		// Inspect the pinned directory's real name, including its ancestors.
		// A short path or a Win32 alias must not hide a protected component.
		resolved, err := legacySnapshotLocalPath(root)
		if err != nil {
			return errors.New("无法核对旧资料在本机磁盘上的实际路径")
		}
		source = resolved
	}
	if pathsOverlap(source, dataDir) || pathsOverlap(source, home) ||
		legacyHomePrivateEntry(filepath.ToSlash(source)) || legacyHomeIsHostDirectory(root) {
		return errors.New("所选目录属于宿主、会话或受限素材范围，请选择具体项目或笔记子目录")
	}
	return nil
}

func legacyHomeIsHostDirectory(root *os.Root) bool {
	file, err := workspacefs.OpenRegular(root, "config.json")
	if err != nil {
		return false
	}
	defer file.Close()
	var fields map[string]json.RawMessage
	if json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&fields) != nil {
		return false
	}
	return fields["companion"] != nil && fields["api_key"] != nil && fields["provider"] != nil
}

func inventoryLegacyHome(ctx context.Context, root *os.Root, maxBytes int64, published bool) (legacyHomeInventory, int64, error) {
	inventory := legacyHomeInventory{Files: []legacyHomeFile{}, Dirs: []string{}}
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("旧资料目录无法完整读取")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." || published && name == legacyHomeManifest {
			return nil
		}
		if !packagecheckout.SafePath(name) || legacyHomePrivateEntry(name) {
			return errors.New("所选目录含宿主配置、会话资料或保留文件，请选择具体的项目子目录")
		}
		info, err := root.Lstat(filepath.FromSlash(name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("资料中有链接或读取期间发生变化")
		}
		if info.IsDir() {
			child, err := workspacefs.OpenExistingDirectory(root, filepath.FromSlash(name))
			if err != nil {
				return errors.New("资料目录含链接或已变化")
			}
			child.Close()
			inventory.Dirs = append(inventory.Dirs, name)
			return nil
		}
		input, err := workspacefs.OpenRegular(root, filepath.FromSlash(name))
		if err != nil {
			return errors.New("所选资料包含不能接续的文件")
		}
		defer input.Close()
		if info.Size() < 0 || info.Size() > math.MaxInt64-total {
			return errors.New("资料总大小超出可处理范围，请拆选子目录")
		}
		total += info.Size()
		if total > maxBytes {
			return fmt.Errorf("已扫描资料至少 %d 字节，超过本次预算 %d 字节；请增加 --legacy-home-max-bytes 或拆选子目录", total, maxBytes)
		}
		hash, copied, err := hashLegacyHomeFile(ctx, input, info.Size())
		if err != nil || copied != info.Size() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("资料读取未完成或文件在读取期间变化")
		}
		inventory.Files = append(inventory.Files, legacyHomeFile{Path: name, Bytes: copied, SHA256: hash})
		return nil
	})
	return inventory, total, err
}

func legacyHomePrivateEntry(name string) bool {
	for _, part := range strings.Split(strings.ToLower(name), "/") {
		part = strings.TrimRight(part, " .")
		if slices.Contains([]string{
			legacyHomeManifest, "api-token", "admin-token", "admin-password", "provider-key",
			"fish-identity.json", "local-model.json", "accounts.json", "bot-state", "account.json", "workspace-directory",
			"conversations", "secrets", "memory.sqlite", "memory.json", "voice-library",
			"media-assets", "media-assets.json", "media-history", "manual-uploads",
			"station-native-stop.json", "station-native-release.json", "legacy-cutover-state.json", "legacy-cutover-claim.json",
		}, part) {
			return true
		}
	}
	return false
}

func ImportLegacyHome(ctx context.Context, dataDir, workspaceRoot, sourceDir, targetRelative string, maxBytes int64, expectedDigest string) (LegacyHomePlan, error) {
	receipt, err := loadLegacyHome(ctx, dataDir, workspaceRoot, sourceDir, targetRelative, maxBytes)
	if err != nil {
		return LegacyHomePlan{}, err
	}
	if receipt.Digest != expectedDigest {
		return LegacyHomePlan{}, errLegacyHomeChanged
	}
	dataDir = filepath.Clean(dataDir)
	source, err := workspacefs.OpenPath(receipt.Source)
	if err != nil {
		return LegacyHomePlan{}, errLegacyHomeChanged
	}
	defer source.Close()
	registry := fishidentity.New(dataDir)
	var parent *os.Root
	parentPath, targetName := filepath.Dir(receipt.Target), filepath.Base(receipt.Target)
	err = registry.GuardHome(receipt.FishID, receipt.OwnerID, receipt.Revision, func(record fishidentity.Record) error {
		home, err := (&localHomeService{root: filepath.Clean(workspaceRoot), dataDir: dataDir}).directory(record)
		if err != nil {
			return err
		}
		if err := validateLegacyHomeSource(source, receipt.Source, dataDir, home); err != nil {
			return err
		}
		var openErr error
		parent, openErr = workspacefs.EnsurePath(parentPath)
		return openErr
	})
	if err != nil {
		return LegacyHomePlan{}, err
	}
	defer parent.Close()
	if matched, err := existingLegacyHome(ctx, parent, targetName, receipt); err != nil {
		return LegacyHomePlan{}, err
	} else if matched {
		err := registry.GuardHome(receipt.FishID, receipt.OwnerID, receipt.Revision, func(fishidentity.Record) error {
			return saveLegacyHomeReceipt(dataDir, receipt)
		})
		return receipt.LegacyHomePlan, err
	}
	stageName, stage, err := packagecheckout.NewStage(parent)
	if err != nil {
		return LegacyHomePlan{}, errors.New("无法准备资料接续目录")
	}
	defer func() { stage.Close(); _ = parent.RemoveAll(stageName) }()
	for _, directory := range receipt.Inventory.Dirs {
		child, err := workspacefs.EnsureDirectory(stage, filepath.FromSlash(directory))
		if err != nil {
			return LegacyHomePlan{}, err
		}
		child.Close()
	}
	var total int64
	for _, file := range receipt.Inventory.Files {
		if err := packagecheckout.CopyCheckedFileWithBudget(ctx, source, stage, file.Path, file.SHA256, &total, maxBytes); err != nil {
			return LegacyHomePlan{}, fmt.Errorf("资料复制未完成：%w", err)
		}
	}
	current, currentTotal, err := inventoryLegacyHome(ctx, source, maxBytes, false)
	if err != nil || currentTotal != receipt.Bytes || !reflect.DeepEqual(current, receipt.Inventory) {
		return LegacyHomePlan{}, errLegacyHomeChanged
	}
	raw, _ := json.Marshal(receipt)
	if err := writeLegacySettingsStageFile(stage, legacyHomeManifest, raw); err != nil {
		return LegacyHomePlan{}, err
	}
	err = registry.GuardHome(receipt.FishID, receipt.OwnerID, receipt.Revision, func(fishidentity.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		liveParent, err := workspacefs.OpenPath(parentPath)
		if err != nil {
			return errLegacyHomeChanged
		}
		defer liveParent.Close()
		before, firstErr := parent.Stat(".")
		after, secondErr := liveParent.Stat(".")
		if firstErr != nil || secondErr != nil || !os.SameFile(before, after) {
			return errLegacyHomeChanged
		}
		stageInfo, stageErr := stage.Stat(".")
		publishedStage, lookupErr := parent.Lstat(stageName)
		if stageErr != nil || lookupErr != nil || !os.SameFile(stageInfo, publishedStage) {
			return errLegacyHomeChanged
		}
		if matched, err := existingLegacyHome(ctx, parent, targetName, receipt); err != nil || matched {
			return err
		}
		return parent.Rename(stageName, targetName)
	})
	if err != nil {
		return LegacyHomePlan{}, err
	}
	return receipt.LegacyHomePlan, saveLegacyHomeReceipt(dataDir, receipt)
}

func existingLegacyHome(ctx context.Context, parent *os.Root, name string, receipt legacyHomeReceipt) (bool, error) {
	if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	directory, err := workspacefs.OpenExistingDirectory(parent, name)
	if err != nil {
		return false, errors.New("接续目标已经存在或经过链接，未覆盖")
	}
	defer directory.Close()
	file, err := workspacefs.OpenRegular(directory, legacyHomeManifest)
	if err != nil {
		return false, errors.New("接续目标已有其他资料，未覆盖")
	}
	var prior legacyHomeReceipt
	err = json.NewDecoder(io.LimitReader(file, 32<<20)).Decode(&prior)
	file.Close()
	if err != nil || !reflect.DeepEqual(prior, receipt) {
		return false, errors.New("接续目标或记录已变化，未覆盖")
	}
	inventory, total, err := inventoryLegacyHome(ctx, directory, receipt.MaxBytes, true)
	if err != nil || total != receipt.Bytes || !reflect.DeepEqual(inventory, receipt.Inventory) {
		return false, errors.New("接续目标已编辑或损坏，未覆盖")
	}
	return true, nil
}

func saveLegacyHomeReceipt(dataDir string, receipt legacyHomeReceipt) error {
	root, err := workspacefs.EnsurePath(filepath.Join(dataDir, "home-imports"))
	if err != nil {
		return errors.New("资料已接续，私有记录暂不可写；可按原摘要重试核对")
	}
	defer root.Close()
	raw, _ := json.Marshal(receipt)
	name := ".receipt-" + identity.RandomToken()
	defer root.Remove(name)
	if err := writeLegacySettingsStageFile(root, name, raw); err != nil {
		return errors.New("资料已接续，私有记录暂不可写；可按原摘要重试核对")
	}
	if err := root.Rename(name, receipt.Digest+".json"); err != nil {
		return errors.New("资料已接续，私有记录暂不可写；可按原摘要重试核对")
	}
	return nil
}

// Hash reads remain cancellable and bounded to the size observed in preflight.
func hashLegacyHomeFile(ctx context.Context, file *os.File, size int64) (string, int64, error) {
	var total int64
	buffer := make([]byte, 64<<10)
	hash := sha256.New()
	for total <= size {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, err := file.Read(buffer)
		total += int64(n)
		_, _ = hash.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			return fmt.Sprintf("%x", hash.Sum(nil)), total, nil
		}
		if err != nil {
			return "", total, err
		}
	}
	return "", total, errLegacyHomeChanged
}
