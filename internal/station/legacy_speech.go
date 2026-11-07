package station

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/packagecheckout"
	"wechat-bot/internal/workspacefs"
)

type legacySpeechOptions struct {
	Version        int                  `json:"schema_version"`
	Library        string               `json:"library"`
	SourceOwner    string               `json:"source_owner_id"`
	AllowUnowned   bool                 `json:"allow_unowned,omitempty"`
	VoiceIDs       []string             `json:"voice_ids"`
	PromptLanguage string               `json:"prompt_language"`
	TextLanguage   string               `json:"text_language"`
	Deployment     localTTSRegistration `json:"deployment"`
}

// LegacySpeechPlan contains selected voice metadata, never reference text,
// executable paths, private model paths or provider credentials.
type LegacySpeechPlan struct {
	Digest        string          `json:"digest"`
	FishID        string          `json:"fish_id"`
	OwnerID       string          `json:"owner_id"`
	OwnerRevision uint64          `json:"owner_revision"`
	SourceOwner   string          `json:"source_owner_id,omitempty"`
	UnownedSource bool            `json:"unowned_source,omitempty"`
	Voices        []localTTSVoice `json:"voices"`
	BytesVerified int64           `json:"bytes_verified"`
	Binding       string          `json:"binding"`
}

type legacySpeechLoaded struct {
	Plan         LegacySpeechPlan
	Registration localTTSRegistration
	Receipt      legacySpeechReceipt
}

const legacySpeechReceiptFile = "legacy-speech-import.json"

// Kept in host-private storage: provenance must survive removal of the
// operator's plan without exposing paths in the chat or CLI result.
type legacySpeechReceipt struct {
	Version            int              `json:"schema_version"`
	Plan               LegacySpeechPlan `json:"plan"`
	Library            string           `json:"library"`
	OptionsSHA256      string           `json:"options_sha256"`
	CatalogSHA256      string           `json:"catalog_sha256"`
	RegistrationSHA256 string           `json:"registration_sha256"`
}

func PreviewLegacySpeech(ctx context.Context, dataDir, workspaceRoot, optionsFile string) (LegacySpeechPlan, error) {
	loaded, err := loadLegacySpeech(ctx, dataDir, workspaceRoot, optionsFile)
	return loaded.Plan, err
}

func loadLegacySpeech(ctx context.Context, dataDir, workspaceRoot, optionsFile string) (legacySpeechLoaded, error) {
	if err := ctx.Err(); err != nil {
		return legacySpeechLoaded{}, err
	}
	for _, directory := range []string{dataDir, workspaceRoot, optionsFile} {
		if !filepath.IsAbs(directory) {
			return legacySpeechLoaded{}, errors.New("声线接续需要私有数据、工作空间和计划文件的绝对路径")
		}
	}
	dataDir, workspaceRoot, optionsFile = filepath.Clean(dataDir), filepath.Clean(workspaceRoot), filepath.Clean(optionsFile)
	if localTTSPathsOverlap(optionsFile, workspaceRoot) {
		return legacySpeechLoaded{}, errors.New("声线接续计划须存放在主人管理的私有目录")
	}
	optionsData, err := localTTSReadRegular(optionsFile, 256<<10)
	if err != nil {
		return legacySpeechLoaded{}, errors.New("声线接续计划不可读")
	}
	var options legacySpeechOptions
	decoder := json.NewDecoder(bytes.NewReader(optionsData))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&options) != nil || decoder.Decode(&struct{}{}) != io.EOF || options.Version != 1 ||
		(options.SourceOwner == "") != options.AllowUnowned || len(options.VoiceIDs) == 0 || len(options.Deployment.Voices) != 0 ||
		!localTTSLanguageAllowed(options.PromptLanguage) || !localTTSLanguageAllowed(options.TextLanguage) {
		return legacySpeechLoaded{}, errors.New("请提供旧主人标识，或明确允许接续未登记主人的条目，并填写声线列表、语言和空 voices 部署模板")
	}
	if !filepath.IsAbs(options.Library) {
		return legacySpeechLoaded{}, errors.New("旧声线库需要绝对路径")
	}
	options.Library = filepath.Clean(options.Library)
	if localTTSPathsOverlap(options.Library, dataDir) || localTTSPathsOverlap(options.Library, workspaceRoot) {
		return legacySpeechLoaded{}, errors.New("旧声线原件须位于宿主配置和鱼家目录之外")
	}
	private, err := workspacefs.OpenPath(dataDir)
	if err != nil {
		return legacySpeechLoaded{}, errors.New("成长站私有目录不可用")
	}
	file, err := workspacefs.OpenRegular(private, "fish-identity.json")
	private.Close()
	if err != nil {
		return legacySpeechLoaded{}, errors.New("请先接续或初始化本机鱼身份")
	}
	file.Close()
	record, err := fishidentity.New(dataDir).Read()
	if err != nil || record.LocalOwner == nil || !record.LocalOwner.HomeAccess {
		return legacySpeechLoaded{}, errors.New("请先为本机主人开启家目录权限")
	}
	library, err := workspacefs.OpenPath(options.Library)
	if err != nil {
		return legacySpeechLoaded{}, errors.New("旧声线库不可用或经过链接")
	}
	defer library.Close()
	catalogData, err := readLegacySettingsFile(library, "catalog.json", true, 8<<20)
	if err != nil {
		return legacySpeechLoaded{}, errors.New("旧声线登记不可读")
	}
	var catalog struct {
		Items []mediaspec.VoiceEntry `json:"items"`
	}
	if json.Unmarshal(catalogData, &catalog) != nil {
		return legacySpeechLoaded{}, errors.New("旧声线登记格式无效")
	}
	voices := map[string]mediaspec.VoiceEntry{}
	for _, entry := range catalog.Items {
		if _, duplicate := voices[entry.ID]; duplicate {
			return legacySpeechLoaded{}, errors.New("旧声线登记含重复 ID，需先核对")
		}
		voices[entry.ID] = entry
	}
	plan := LegacySpeechPlan{
		FishID: record.FishID, OwnerID: record.LocalOwner.ID, OwnerRevision: record.LocalOwner.Revision,
		SourceOwner: options.SourceOwner, UnownedSource: options.AllowUnowned, Voices: []localTTSVoice{},
	}
	registration := options.Deployment
	for _, resource := range []*string{&registration.Project, &registration.Python, &registration.BaseModels, &registration.Runner, &registration.Packages, &registration.Media, &registration.NLTKData} {
		if *resource != "" {
			*resource = filepath.Clean(*resource)
		}
	}
	if registration.SchemaVersion != 1 || !registration.Enabled {
		return legacySpeechLoaded{}, errors.New("请明确登记版本为 1 的已授权语音部署")
	}
	seen := map[string]bool{}
	for _, id := range options.VoiceIDs {
		entry, ok := voices[id]
		if !ok || seen[id] || entry.Owner != options.SourceOwner || !strings.EqualFold(entry.Engine, "GPT-SoVITS") {
			return legacySpeechLoaded{}, errors.New("选定声线缺失、归属不符、重复或不是 GPT-SoVITS")
		}
		seen[id] = true
		voice, size, err := legacySpeechVoice(ctx, library, options.Library, entry, options.PromptLanguage, options.TextLanguage)
		if err != nil {
			return legacySpeechLoaded{}, fmt.Errorf("声线 %s 无法接续：%w", id, err)
		}
		if size > math.MaxInt64-plan.BytesVerified {
			return legacySpeechLoaded{}, errors.New("声线资源总量超出可处理范围")
		}
		plan.BytesVerified += size
		registration.Voices = append(registration.Voices, voice)
		plan.Voices = append(plan.Voices, voice.view())
	}
	if err := registration.validate(dataDir, workspaceRoot); err != nil {
		return legacySpeechLoaded{}, fmt.Errorf("语音运行资源登记无效：%w", err)
	}
	slices.SortFunc(registration.Voices, func(a, b localTTSVoiceRegistration) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(plan.Voices, func(a, b localTTSVoice) int { return strings.Compare(a.ID, b.ID) })
	plan.Binding = localTTSBinding(registration)
	raw, _ := json.Marshal([]any{"legacy-speech-v1", dataDir, workspaceRoot, optionsFile, legacySettingsHash(optionsData), legacySettingsHash(catalogData), plan})
	plan.Digest = legacySettingsHash(raw)
	registrationData, _ := json.Marshal(registration)
	if len(registrationData) > localTTSRegistrationMax {
		return legacySpeechLoaded{}, errors.New("语音登记超过 192 KiB，请减少本次选定声线")
	}
	return legacySpeechLoaded{Plan: plan, Registration: registration, Receipt: legacySpeechReceipt{
		Version: 1, Plan: plan, Library: options.Library,
		OptionsSHA256: legacySettingsHash(optionsData), CatalogSHA256: legacySettingsHash(catalogData),
		RegistrationSHA256: legacySettingsHash(registrationData),
	}}, nil
}

func legacySpeechVoice(ctx context.Context, root *os.Root, library string, entry mediaspec.VoiceEntry, promptLanguage, textLanguage string) (localTTSVoiceRegistration, int64, error) {
	selectFile := func(kind string, files []mediaspec.VoiceFile) (mediaspec.VoiceFile, error) {
		candidates := []mediaspec.VoiceFile{}
		recommended := entry.Recommended[kind]
		for _, file := range files {
			match := file.Kind == kind
			if kind == "reference" {
				match = slices.Contains([]string{".wav", ".mp3", ".flac", ".m4a"}, strings.ToLower(filepath.Ext(file.Filename)))
			} else if kind == "prompt" {
				match = strings.EqualFold(filepath.Ext(file.Filename), ".txt")
			}
			if !match {
				continue
			}
			fileID := file.File
			if fileID == "" {
				fileID = file.Hash + filepath.Ext(file.Filename)
			}
			if recommended != "" && fileID != recommended {
				continue
			}
			candidates = append(candidates, file)
		}
		if len(candidates) != 1 {
			return mediaspec.VoiceFile{}, fmt.Errorf("%s 需要唯一或已登记的推荐文件", kind)
		}
		return candidates[0], nil
	}
	voice := localTTSVoiceRegistration{ID: entry.ID, Label: entry.Name, PromptLanguage: promptLanguage, TextLanguage: textLanguage}
	var size int64
	for _, selected := range []struct {
		kind       string
		files      []mediaspec.VoiceFile
		dest, hash *string
	}{
		{"gpt", entry.Weights, &voice.GPT, &voice.GPTSHA256},
		{"sovits", entry.Weights, &voice.SoVITS, &voice.SoVITSSHA256},
		{"reference", entry.References, &voice.Reference, &voice.ReferenceSHA256},
		{"prompt", entry.References, &voice.Prompt, &voice.PromptSHA256},
	} {
		item, err := selectFile(selected.kind, selected.files)
		if err != nil {
			return voice, 0, err
		}
		name := filepath.ToSlash(item.Relative)
		if name == "" {
			if !packagecheckout.SafePath(item.File) || path.Base(item.File) != item.File {
				return voice, 0, errors.New("旧资源编号不是单个文件名")
			}
			name = "files/" + item.File
		}
		if !packagecheckout.SafePath(name) || item.Bytes <= 0 || item.Bytes == math.MaxInt64 || !localTTSHexPattern.MatchString(item.Hash) {
			return voice, 0, errors.New("资源路径、大小或哈希未登记")
		}
		file, err := workspacefs.OpenRegular(root, filepath.FromSlash(name))
		if err != nil {
			return voice, 0, errors.New("声线资源不存在或经过链接")
		}
		hash, count, err := hashLegacyHomeFile(ctx, file, item.Bytes)
		file.Close()
		if ctx.Err() != nil {
			return voice, 0, ctx.Err()
		}
		if err != nil || count != item.Bytes || hash != item.Hash || count > math.MaxInt64-size {
			return voice, 0, errors.New("声线资源与登记的大小或哈希不符")
		}
		*selected.dest, *selected.hash = filepath.Join(library, filepath.FromSlash(name)), item.Hash
		size += count
	}
	return voice, size, nil
}

func ImportLegacySpeech(ctx context.Context, dataDir, workspaceRoot, optionsFile, expectedDigest string) (LegacySpeechPlan, error) {
	loaded, err := loadLegacySpeech(ctx, dataDir, workspaceRoot, optionsFile)
	if err != nil {
		return LegacySpeechPlan{}, err
	}
	if loaded.Plan.Digest != expectedDigest {
		return LegacySpeechPlan{}, errors.New("声线接续来源、运行资源或主人授权已变化，请重新预演")
	}
	dataDir = filepath.Clean(dataDir)
	err = fishidentity.New(dataDir).GuardHome(loaded.Plan.FishID, loaded.Plan.OwnerID, loaded.Plan.OwnerRevision, func(fishidentity.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		root, err := workspacefs.OpenPath(dataDir)
		if err != nil {
			return errors.New("宿主私有目录不可用")
		}
		defer root.Close()
		installed := false
		if file, err := workspacefs.OpenRegular(root, localTTSRegistrationFile); err == nil {
			var current localTTSRegistration
			raw, readErr := io.ReadAll(io.LimitReader(file, localTTSRegistrationMax+1))
			file.Close()
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if readErr != nil || len(raw) > localTTSRegistrationMax || decoder.Decode(&current) != nil || decoder.Decode(&struct{}{}) != io.EOF || !reflect.DeepEqual(current, loaded.Registration) {
				return errors.New("新站已登记其他语音配置，原配置未覆盖")
			}
			installed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("新站语音登记不可读或经过链接")
		}
		// Persist provenance first. If publishing the registration fails, retry
		// the same digest and finish it; never enable an unjournaled migration.
		if err := saveLegacySpeechReceipt(root, loaded.Receipt); err != nil {
			return err
		}
		if installed {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, _ := json.Marshal(loaded.Registration)
		temp := ".speech-import-" + identity.RandomToken()
		defer root.Remove(temp)
		if err := writeLegacySettingsStageFile(root, temp, raw); err != nil {
			return err
		}
		return root.Rename(temp, localTTSRegistrationFile)
	})
	return loaded.Plan, err
}

func saveLegacySpeechReceipt(root *os.Root, receipt legacySpeechReceipt) error {
	if file, err := workspacefs.OpenRegular(root, legacySpeechReceiptFile); err == nil {
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, localTTSRegistrationMax+1))
		var prior legacySpeechReceipt
		if err != nil || len(raw) > localTTSRegistrationMax || json.Unmarshal(raw, &prior) != nil || !reflect.DeepEqual(prior, receipt) {
			return errors.New("已存在其他声线接续记录，请核对原计划")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("声线接续记录不可读或经过链接")
	}
	raw, _ := json.Marshal(receipt)
	if len(raw) > localTTSRegistrationMax {
		return errors.New("声线接续记录过大，请减少本次选定声线")
	}
	temp := ".speech-receipt-" + identity.RandomToken()
	defer root.Remove(temp)
	if err := writeLegacySettingsStageFile(root, temp, raw); err != nil {
		return err
	}
	return root.Rename(temp, legacySpeechReceiptFile)
}
