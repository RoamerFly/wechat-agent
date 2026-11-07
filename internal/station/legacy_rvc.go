package station

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/packagecheckout"
	"wechat-bot/internal/strictjson"
	"wechat-bot/internal/workspacefs"
)

const legacyRVCDefaultBytes = 4 << 30

type legacyRVCOptions struct {
	Version      int                  `json:"schema_version"`
	Library      string               `json:"library"`
	SourceOwner  string               `json:"source_owner_id"`
	AllowUnowned bool                 `json:"allow_unowned,omitempty"`
	VoiceIDs     []string             `json:"voice_ids"`
	MaxBytes     int64                `json:"max_bytes,omitempty"`
	Deployment   localRVCRegistration `json:"deployment"`
}

// LegacyRVCPlan describes the exact profile mapping and copy size. Private
// resource locations, hashes of individual weights and settings stay on disk.
type LegacyRVCPlan struct {
	Digest                 string          `json:"digest"`
	FishID                 string          `json:"fish_id"`
	OwnerID                string          `json:"owner_id"`
	OwnerRevision          uint64          `json:"owner_revision"`
	UnownedSource          bool            `json:"unowned_source,omitempty"`
	CatalogPathDifferences int             `json:"catalog_path_differences,omitempty"`
	Profiles               []localRVCVoice `json:"profiles"`
	Bytes                  int64           `json:"bytes"`
	MaxBytes               int64           `json:"max_bytes"`
}

type legacyRVCObject struct {
	Path string
	Hash string
	Size int64
}

type legacyRVCLoaded struct {
	Plan         LegacyRVCPlan
	Registration localRVCRegistration
	Source       string
	SourceOwner  string
	Target       string
	Objects      []legacyRVCObject
	Files        map[string][]byte
	Origin       map[string]string
}

// Only fields used by the RVC runtime cross the old configuration boundary.
type legacyRVCPreset struct {
	Enabled                bool     `json:"enabled"`
	Engine                 string   `json:"engine"`
	Profile                string   `json:"profile"`
	Python                 string   `json:"python"`
	Runtime                string   `json:"runtime"`
	RVCPython              string   `json:"rvc_python"`
	Project                string   `json:"rvc_project"`
	Model                  string   `json:"rvc_model"`
	Index                  string   `json:"rvc_index"`
	ModelSHA256            string   `json:"rvc_model_sha256"`
	IndexSHA256            string   `json:"rvc_index_sha256"`
	Device                 string   `json:"rvc_device"`
	IndexRate              *float64 `json:"index_rate,omitempty"`
	Semitones              int      `json:"semitones,omitempty"`
	AccompanimentSemitones int      `json:"accompaniment_semitones,omitempty"`
	Protect                *float64 `json:"protect,omitempty"`
	Precision              string   `json:"precision,omitempty"`
}

type legacyRVCManifest struct {
	Configuration string `json:"configuration"`
}

func PreviewLegacyRVC(ctx context.Context, dataDir, workspaceRoot, optionsFile string) (LegacyRVCPlan, error) {
	loaded, err := loadLegacyRVC(ctx, dataDir, workspaceRoot, optionsFile)
	return loaded.Plan, err
}

func loadLegacyRVC(ctx context.Context, dataDir, workspaceRoot, optionsFile string) (legacyRVCLoaded, error) {
	if err := ctx.Err(); err != nil {
		return legacyRVCLoaded{}, err
	}
	for _, name := range []string{dataDir, workspaceRoot, optionsFile} {
		if !filepath.IsAbs(name) {
			return legacyRVCLoaded{}, errors.New("RVC 接续需要数据、工作空间和计划的绝对路径")
		}
	}
	dataDir, workspaceRoot, optionsFile = filepath.Clean(dataDir), filepath.Clean(workspaceRoot), filepath.Clean(optionsFile)
	if localRVCPathsOverlap(optionsFile, workspaceRoot) || localRVCInside(dataDir, workspaceRoot) {
		return legacyRVCLoaded{}, errors.New("接续计划和宿主数据须在鱼家工作空间之外")
	}
	raw, err := localRVCReadRegular(optionsFile, 256<<10)
	if err != nil {
		return legacyRVCLoaded{}, errors.New("RVC 接续计划不可读")
	}
	var options legacyRVCOptions
	if strictjson.DecodeKnownFields(raw, &options) != nil || options.Version != 1 ||
		options.SourceOwner != strings.TrimSpace(options.SourceOwner) || (options.SourceOwner == "") != options.AllowUnowned ||
		len(options.VoiceIDs) == 0 || !filepath.IsAbs(options.Library) || !filepath.IsAbs(options.Deployment.Workspace) ||
		!filepath.IsAbs(options.Deployment.Media) || options.Deployment.SchemaVersion != 1 || !options.Deployment.Enabled ||
		len(options.Deployment.Profiles) != 0 || options.Deployment.FishID != "" {
		return legacyRVCLoaded{}, errors.New("请明确旧声线库、旧主人或无主人条目授权、声线编号和版本为 1 的 RVC 部署，profiles 与 fish_id 由接续生成")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = legacyRVCDefaultBytes
	}
	if options.MaxBytes < 0 || options.MaxBytes == math.MaxInt64 {
		return legacyRVCLoaded{}, errors.New("RVC 接续字节预算无效")
	}
	source := filepath.Clean(options.Library)
	if localRVCPathsOverlap(source, dataDir) || localRVCPathsOverlap(source, workspaceRoot) {
		return legacyRVCLoaded{}, errors.New("旧声线库须在新站数据和鱼家工作空间之外")
	}
	private, err := workspacefs.OpenPath(dataDir)
	if err != nil {
		return legacyRVCLoaded{}, errors.New("成长站私有目录不可用")
	}
	identityFile, err := workspacefs.OpenRegular(private, "fish-identity.json")
	private.Close()
	if err != nil {
		return legacyRVCLoaded{}, errors.New("请先接续或初始化本机鱼身份")
	}
	identityFile.Close()
	record, err := fishidentity.New(dataDir).Read()
	if err != nil || record.LocalOwner == nil || !record.LocalOwner.HomeAccess {
		return legacyRVCLoaded{}, errors.New("请先为本机主人开启家目录权限")
	}
	reg := options.Deployment
	reg.Workspace, reg.Media, reg.FishID = filepath.Clean(reg.Workspace), filepath.Clean(reg.Media), record.FishID
	target := filepath.Join(localRVCPrivateScope(reg.Workspace, record.FishID), "voice-library")
	if localRVCPathsOverlap(target, source) || localRVCPathsOverlap(optionsFile, target) {
		return legacyRVCLoaded{}, errors.New("旧声线库和接续计划须与目标私有声线库分开")
	}
	if err := reg.validate(dataDir, workspaceRoot); err != nil {
		return legacyRVCLoaded{}, fmt.Errorf("RVC 运行环境登记无效：%w", err)
	}
	library, err := workspacefs.OpenPath(source)
	if err != nil {
		return legacyRVCLoaded{}, errors.New("旧声线库不可用或经过链接")
	}
	defer library.Close()
	loaded := legacyRVCLoaded{
		Plan:         LegacyRVCPlan{FishID: record.FishID, OwnerID: record.LocalOwner.ID, OwnerRevision: record.LocalOwner.Revision, MaxBytes: options.MaxBytes, UnownedSource: options.AllowUnowned},
		Registration: reg, Source: source, SourceOwner: options.SourceOwner, Target: target, Files: map[string][]byte{},
		Origin: map[string]string{},
	}
	catalogData, err := loaded.readSource(library, "catalog.json", 8<<20)
	var catalog struct {
		Items []mediaspec.VoiceEntry `json:"items"`
	}
	if err != nil || json.Unmarshal(catalogData, &catalog) != nil {
		return legacyRVCLoaded{}, errors.New("旧声线目录不可读")
	}
	entries := make(map[string]mediaspec.VoiceEntry, len(catalog.Items))
	for _, entry := range catalog.Items {
		if _, duplicate := entries[entry.ID]; duplicate {
			return legacyRVCLoaded{}, errors.New("旧声线库含重复声线 ID")
		}
		entries[entry.ID] = entry
	}
	seen := map[string]bool{}
	objects := map[string]legacyRVCObject{}
	for _, id := range options.VoiceIDs {
		entry := entries[id]
		if entry.Owner != options.SourceOwner || !strings.EqualFold(entry.Engine, "rvc") ||
			!localTTSVoiceIDPattern.MatchString(entry.ID) || !localTTSSafeLabel(entry.Name) ||
			!strings.HasPrefix(entry.ProfileID, "private-") || !localRVCVoicePattern.MatchString(entry.ProfileID) ||
			seen[strings.ToLower(entry.ProfileID)] {
			return legacyRVCLoaded{}, errors.New("选定声线缺失、归属不符、重复或没有私人 RVC 配置")
		}
		seen[strings.ToLower(entry.ProfileID)] = true
		if err := loaded.addProfile(ctx, library, entry, objects); err != nil {
			return legacyRVCLoaded{}, err
		}
	}
	slices.SortFunc(loaded.Objects, func(a, b legacyRVCObject) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(loaded.Registration.Profiles, func(a, b localRVCVoice) int { return strings.Compare(a.ID, b.ID) })
	if err := loaded.Registration.validate(dataDir, workspaceRoot); err != nil {
		return legacyRVCLoaded{}, err
	}
	loaded.Plan.Profiles = loaded.Registration.Profiles
	registration, _ := json.Marshal(loaded.Registration)
	if len(registration) > 64<<10 {
		return legacyRVCLoaded{}, errors.New("RVC 登记超过 64 KiB，请缩小声线选择范围")
	}
	binding, _ := json.Marshal([]any{"legacy-rvc-v1", dataDir, workspaceRoot, optionsFile, source, target,
		legacySettingsHash(raw), loaded.Plan, loaded.Origin, loaded.Objects, loaded.Files, loaded.Registration})
	loaded.Plan.Digest = legacySettingsHash(binding)
	return loaded, nil
}

func (l *legacyRVCLoaded) readSource(root *os.Root, name string, limit int64) ([]byte, error) {
	data, err := readLegacySettingsFile(root, filepath.FromSlash(name), true, limit)
	if err == nil {
		l.Origin[name] = legacySettingsHash(data)
	}
	return bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), err
}

func (l *legacyRVCLoaded) addProfile(ctx context.Context, root *os.Root, entry mediaspec.VoiceEntry, objects map[string]legacyRVCObject) error {
	manifestPath := "manifests/" + entry.ProfileID + ".json"
	raw, err := l.readSource(root, manifestPath, 64<<10)
	var manifest legacyRVCManifest
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		return errors.New("旧 RVC 配置清单不可读")
	}
	configPath, err := legacyRVCRelative(manifest.Configuration, l.Source)
	if err != nil {
		return errors.New("旧 RVC 运行清单指向私有声线库之外")
	}
	// The old bridge resolves the exact profile through this manifest. A
	// catalogue path is display metadata and can be stale after relocation.
	if entry.Configuration != "" && !legacyRVCSamePath(entry.Configuration, manifest.Configuration) {
		l.Plan.CatalogPathDifferences++
	}
	raw, err = l.readSource(root, configPath, 64<<10)
	var preset legacyRVCPreset
	if err != nil || json.Unmarshal(raw, &preset) != nil || !preset.valid() {
		return errors.New("旧 RVC 配置无效，无法保持原换声参数")
	}
	if err := preset.checkRuntime(filepath.Join(l.Registration.Workspace, "public_workspace")); err != nil {
		return err
	}
	for _, object := range []struct {
		name *string
		hash string
	}{{&preset.Model, preset.ModelSHA256}, {&preset.Index, preset.IndexSHA256}} {
		name, err := legacyRVCRelative(*object.name, l.Source)
		if err != nil || !localRVCRequestPattern.MatchString(object.hash) {
			return errors.New("RVC 权重或索引不在旧私有声线库内，或缺少哈希")
		}
		if existing, ok := objects[strings.ToLower(name)]; ok {
			if existing.Hash != object.hash || existing.Path != name {
				return errors.New("旧 RVC 资源路径冲突")
			}
		} else {
			size, err := checkLegacyRVCObject(ctx, root, name, object.hash, l.Plan.MaxBytes-l.Plan.Bytes)
			if err != nil {
				return err
			}
			item := legacyRVCObject{name, object.hash, size}
			l.Plan.Bytes += size
			l.Objects = append(l.Objects, item)
			objects[strings.ToLower(name)] = item
		}
		*object.name = filepath.Join(l.Target, "resources", filepath.FromSlash(name))
	}
	config := "profiles/" + entry.ProfileID + ".json"
	l.Files[config], _ = json.Marshal(preset)
	l.Files[manifestPath], _ = json.Marshal(legacyRVCManifest{Configuration: filepath.Join(l.Target, filepath.FromSlash(config))})
	l.Registration.Profiles = append(l.Registration.Profiles, localRVCVoice{ID: entry.ProfileID, Label: entry.Name, LegacyVoiceID: entry.ID})
	return nil
}

func (p *legacyRVCPreset) valid() bool {
	if p.Device == "" {
		p.Device = "auto"
	}
	return p.Enabled && p.Engine == "rvc" && localRVCVoicePattern.MatchString(p.Profile) &&
		slices.Contains([]string{"auto", "cpu", "cuda:0"}, p.Device) &&
		slices.Contains([]string{"", "auto", "float32"}, p.Precision) &&
		p.Semitones >= -12 && p.Semitones <= 12 &&
		p.AccompanimentSemitones >= -12 && p.AccompanimentSemitones <= 12 &&
		(p.Semitones-p.AccompanimentSemitones)%12 == 0 &&
		(p.IndexRate == nil || (*p.IndexRate >= 0 && *p.IndexRate <= 1)) &&
		(p.Protect == nil || (*p.Protect >= 0 && *p.Protect <= .5))
}

func (p *legacyRVCPreset) checkRuntime(public string) error {
	runtimes := filepath.Join(public, "runtimes")
	projects := filepath.Join(public, "projects")
	for _, executable := range []*string{&p.Python, &p.RVCPython} {
		*executable = filepath.Clean(*executable)
		if !localRVCInside(*executable, runtimes) || !strings.EqualFold(filepath.Ext(*executable), ".exe") || !localRVCSafeRegular(*executable) {
			return errors.New("旧 RVC 解释器不在获准运行环境内")
		}
	}
	p.Runtime, p.Project = filepath.Clean(p.Runtime), filepath.Clean(p.Project)
	if !localRVCSafeDirectory(p.Runtime) || (!localRVCInside(p.Runtime, runtimes) && !legacyRVCSamePath(p.Runtime, filepath.Join(public, "models", "separator"))) ||
		!localRVCSafeDirectory(p.Project) || !localRVCInside(p.Project, projects) {
		return errors.New("旧 RVC 项目或分离运行环境不在获准目录内")
	}
	hasFile := func(names ...string) bool {
		for _, name := range names {
			if localRVCSafeRegular(filepath.Join(p.Project, filepath.FromSlash(name))) {
				return true
			}
		}
		return false
	}
	if !hasFile("assets/hubert_base/pytorch_model.bin", "assets/hubert/hubert_base.pt") ||
		!hasFile("assets/rmvpe/rmvpe.pt") || !hasFile("infer/vc/modules.py", "infer/modules/vc/modules.py") {
		return errors.New("旧 RVC 项目缺少 HuBERT、RMVPE 或推理入口")
	}
	return nil
}

func legacyRVCSamePath(a, b string) bool {
	rel, err := filepath.Rel(filepath.Clean(a), filepath.Clean(b))
	return err == nil && rel == "."
}

func legacyRVCRelative(value, root string) (string, error) {
	if !filepath.IsAbs(value) || !localRVCInside(value, root) {
		return "", errors.New("旧 RVC 资源超出私有声线库")
	}
	rel, err := filepath.Rel(root, filepath.Clean(value))
	name := filepath.ToSlash(rel)
	if err != nil || name == "." || !packagecheckout.SafePath(name) {
		return "", errors.New("旧 RVC 资源路径无效")
	}
	return name, nil
}

func checkLegacyRVCObject(ctx context.Context, root *os.Root, name, hash string, budget int64) (int64, error) {
	file, err := workspacefs.OpenRegular(root, filepath.FromSlash(name))
	if err != nil {
		return 0, errors.New("RVC 模型或索引缺失或经过链接")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 {
		return 0, errors.New("RVC 模型或索引大小无效")
	}
	if info.Size() > budget {
		return 0, fmt.Errorf("RVC 模型或索引需要 %d 字节，接续预算仅余 %d 字节，请提高 max_bytes 或缩小声线范围", info.Size(), budget)
	}
	actual, count, err := hashLegacyHomeFile(ctx, file, info.Size())
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil || count != info.Size() || actual != hash {
		return 0, errors.New("RVC 模型或索引与原登记哈希不符")
	}
	return count, nil
}

const legacyRVCImportReceipt = ".station-rvc-import.json"

type legacyRVCReceipt struct {
	Version     int               `json:"schema_version"`
	Plan        LegacyRVCPlan     `json:"plan"`
	Library     string            `json:"library"`
	SourceOwner string            `json:"source_owner_id"`
	Origin      map[string]string `json:"source_sha256"`
	Objects     []legacyRVCObject `json:"objects"`
}

func ImportLegacyRVC(ctx context.Context, dataDir, workspaceRoot, optionsFile, expectedDigest string) (LegacyRVCPlan, error) {
	loaded, err := loadLegacyRVC(ctx, dataDir, workspaceRoot, optionsFile)
	if err != nil {
		return LegacyRVCPlan{}, err
	}
	if loaded.Plan.Digest != expectedDigest {
		return LegacyRVCPlan{}, errors.New("RVC 接续来源、运行登记或主人授权已变化，请重新预演")
	}
	loaded.Files[legacyRVCImportReceipt], _ = json.Marshal(legacyRVCReceipt{
		Version: 1, Plan: loaded.Plan, Library: loaded.Source, SourceOwner: loaded.SourceOwner,
		Origin: loaded.Origin, Objects: loaded.Objects,
	})
	dataDir = filepath.Clean(dataDir)
	private, err := workspacefs.OpenPath(dataDir)
	if err != nil {
		return LegacyRVCPlan{}, errors.New("成长站私有目录不可用")
	}
	defer private.Close()
	registration, _ := json.Marshal(loaded.Registration)
	registered, err := legacyRVCExistingRegistration(private, loaded.Registration)
	if err != nil {
		return LegacyRVCPlan{}, err
	}

	registry := fishidentity.New(dataDir)
	parentPath, name := filepath.Dir(loaded.Target), filepath.Base(loaded.Target)
	var parent *os.Root
	if err := registry.GuardHome(loaded.Plan.FishID, loaded.Plan.OwnerID, loaded.Plan.OwnerRevision, func(fishidentity.Record) error {
		var err error
		parent, err = workspacefs.EnsurePath(parentPath)
		return err
	}); err != nil {
		return LegacyRVCPlan{}, err
	}
	defer parent.Close()

	matched, err := loaded.verifyTarget(ctx, parent, name)
	if err != nil {
		return LegacyRVCPlan{}, err
	}
	if registered && !matched {
		return LegacyRVCPlan{}, errors.New("已登记 RVC 的私有声线库缺失，请先核对原接续资料")
	}
	var staged string
	if !matched {
		staged, err = loaded.stage(ctx, parent)
		if err != nil {
			return LegacyRVCPlan{}, err
		}
		defer parent.RemoveAll(staged)
	}
	err = registry.GuardHome(loaded.Plan.FishID, loaded.Plan.OwnerID, loaded.Plan.OwnerRevision, func(fishidentity.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !legacyRVCParentUnchanged(parent, parentPath) || !legacyRVCParentUnchanged(private, dataDir) {
			return errors.New("RVC 接续目录已变化")
		}
		registered, err := legacyRVCExistingRegistration(private, loaded.Registration)
		if err != nil {
			return err
		}
		existing, err := loaded.verifyTarget(ctx, parent, name)
		if err != nil {
			return err
		}
		if !existing {
			if staged == "" || registered {
				return errors.New("RVC 接续目标已变化")
			}
			if err := parent.Rename(staged, name); err != nil {
				return errors.New("RVC 私有声线库启用失败，请核对目标后重试")
			}
		}
		if registered {
			return nil
		}
		// A completed library and its receipt can survive a failed registration
		// write. The same digest verifies the published files before retrying.
		temp := ".rvc-import-" + identity.RandomToken()
		defer private.Remove(temp)
		if err := writeLegacySettingsStageFile(private, temp, registration); err != nil {
			return errors.New("RVC 声线已接续，写入运行登记失败，可用原摘要重试")
		}
		if err := private.Rename(temp, localRVCRegistrationFile); err != nil {
			return errors.New("RVC 声线已接续，启用运行登记失败，可用原摘要重试")
		}
		return nil
	})
	return loaded.Plan, err
}

func legacyRVCExistingRegistration(root *os.Root, expected localRVCRegistration) (bool, error) {
	raw, err := readLegacySettingsFile(root, localRVCRegistrationFile, false, 64<<10)
	if err != nil {
		return false, errors.New("现有 RVC 运行登记不可读或经过链接")
	}
	if raw == nil {
		return false, nil
	}
	var current localRVCRegistration
	if strictjson.DecodeKnownFields(raw, &current) != nil || !reflect.DeepEqual(current, expected) {
		return false, errors.New("新站已有其他 RVC 运行登记，未覆盖")
	}
	return true, nil
}

func legacyRVCParentUnchanged(root *os.Root, path string) bool {
	live, err := workspacefs.OpenPath(path)
	if err != nil {
		return false
	}
	defer live.Close()
	pinned, pinErr := root.Stat(".")
	current, liveErr := live.Stat(".")
	return pinErr == nil && liveErr == nil && os.SameFile(pinned, current)
}

func (l legacyRVCLoaded) verifyTarget(ctx context.Context, parent *os.Root, name string) (bool, error) {
	if _, err := parent.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	target, err := workspacefs.OpenExistingDirectory(parent, name)
	if err != nil {
		return false, errors.New("RVC 接续目标已有其他资料或经过链接，未覆盖")
	}
	defer target.Close()
	// Verify the receipt before reading anything from an existing library.
	receipt := l.Files[legacyRVCImportReceipt]
	raw, err := readLegacySettingsFile(target, legacyRVCImportReceipt, true, int64(len(receipt)))
	if err != nil || !bytes.Equal(raw, receipt) {
		return false, errors.New("目标私有声线库的接续记录不匹配，未覆盖")
	}
	for name, expected := range l.Files {
		if name == legacyRVCImportReceipt {
			continue
		}
		actual, err := readLegacySettingsFile(target, filepath.FromSlash(name), true, int64(len(expected)))
		if err != nil || !bytes.Equal(actual, expected) {
			return false, errors.New("目标 RVC 配置已变化，未覆盖")
		}
	}
	for _, object := range l.Objects {
		name := "resources/" + object.Path
		size, err := checkLegacyRVCObject(ctx, target, name, object.Hash, object.Size)
		if err != nil || size != object.Size {
			return false, errors.New("目标 RVC 权重或索引已变化，未覆盖")
		}
	}
	return true, nil
}

func (l legacyRVCLoaded) stage(ctx context.Context, parent *os.Root) (name string, err error) {
	source, err := workspacefs.OpenPath(l.Source)
	if err != nil {
		return "", errors.New("旧声线库已变化")
	}
	defer source.Close()
	stageName, stage, err := packagecheckout.NewStage(parent)
	if err != nil {
		return "", errors.New("无法创建 RVC 声线暂存目录")
	}
	defer func() {
		stage.Close()
		if err != nil {
			_ = parent.RemoveAll(stageName)
		}
	}()
	resources, err := workspacefs.EnsureDirectory(stage, "resources")
	if err != nil {
		return "", err
	}
	defer resources.Close()
	var copied int64
	for _, object := range l.Objects {
		if err := packagecheckout.CopyCheckedFileWithBudget(ctx, source, resources, object.Path, object.Hash, &copied, l.Plan.MaxBytes); err != nil {
			return "", errors.New("复制 RVC 模型或索引失败，请核对源文件与目标磁盘")
		}
	}
	for name, expected := range l.Origin {
		limit := int64(64 << 10)
		if name == "catalog.json" {
			limit = 8 << 20
		}
		raw, err := readLegacySettingsFile(source, filepath.FromSlash(name), true, limit)
		if err != nil || legacySettingsHash(raw) != expected {
			return "", errors.New("RVC 旧配置在复制期间已变化，请重新预演")
		}
	}
	for name, data := range l.Files {
		name = filepath.FromSlash(name)
		directory, err := workspacefs.EnsureDirectory(stage, filepath.Dir(name))
		if err != nil {
			return "", err
		}
		err = writeLegacySettingsStageFile(directory, filepath.Base(name), data)
		directory.Close()
		if err != nil {
			return "", err
		}
	}
	return stageName, nil
}
