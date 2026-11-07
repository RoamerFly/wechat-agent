package station

// This file deliberately imports only the two installation-level records that
// can be continued without giving the new station any old application grant:
// the model settings and the fish identity.  WeChat state, queues, account
// scopes and all application permissions have their own opt-in migrations.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/workspacefs"
)

const (
	legacySettingsConfigFile   = "config.json"
	legacySettingsIdentityFile = "fish-identity.json"
	legacySettingsReceiptFile  = "legacy-settings-import.json"
	// Old config and identity records have independent historical limits.
	// The new local-model record remains deliberately smaller.
	legacySettingsSourceLimit = 1 << 20
	legacySettingsTargetLimit = 128 << 10
)

var (
	errLegacySettingsPath     = errors.New("旧设置接续路径无效")
	errLegacySettingsChanged  = errors.New("旧设置已变化，请重新预演")
	errLegacySettingsConflict = errors.New("目标目录已存在或接续记录不匹配，请选择全新数据目录")
)

// LegacySettingsPlan is intentionally safe to render in the UI.  It never
// contains API keys, personas, or raw identity content.
type LegacySettingsPlan struct {
	Digest      string                  `json:"digest"`
	Source      string                  `json:"source"`
	Target      string                  `json:"target"`
	Model       LegacyModelSettingsView `json:"model"`
	HasIdentity bool                    `json:"has_identity"`
	FishID      string                  `json:"fish_id,omitempty"`
}

type LegacyModelSettingsView struct {
	Provider         string `json:"provider"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	MaxTokens        int    `json:"max_tokens"`
	ThinkingEnabled  bool   `json:"thinking_enabled"`
	ReasoningEffort  string `json:"reasoning_effort"`
	KeyConfigured    bool   `json:"key_configured"`
	PersonaSet       bool   `json:"persona_set"`
	WechatPersonaSet bool   `json:"wechat_persona_set"`
}

type legacySettingsConfig struct {
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	SystemPrompt    string `json:"system_prompt"`
	MaxTokens       int    `json:"max_tokens"`
	ThinkingEnabled bool   `json:"thinking_enabled"`
	ReasoningEffort string `json:"reasoning_effort"`
	APIKey          string `json:"api_key"`
	Companion       struct {
		Persona      string `json:"persona"`
		Style        string `json:"style"`
		Relationship string `json:"relationship"`
	} `json:"companion"`
}

type legacySettingsReceipt struct {
	Version        int                     `json:"version"`
	Digest         string                  `json:"digest"`
	Source         string                  `json:"source"`
	ConfigSHA256   string                  `json:"config_sha256"`
	IdentitySHA256 string                  `json:"identity_sha256,omitempty"`
	Model          LegacyModelSettingsView `json:"model"`
	FishID         string                  `json:"fish_id,omitempty"`
}

type legacySettingsLoaded struct {
	plan       LegacySettingsPlan
	record     localModelRecord
	identity   []byte
	configHash string
	idHash     string
	appPersona string
}

// PreviewLegacySettings reads a stopped legacy admin directory without
// writing either the source or destination.  Its digest binds both canonical
// directories and exact input contents, so ImportLegacySettings cannot apply
// a plan to a different station or a stale snapshot.
func PreviewLegacySettings(ctx context.Context, sourceAdminDir, targetDir string) (LegacySettingsPlan, error) {
	loaded, err := loadLegacySettings(ctx, sourceAdminDir, targetDir)
	if err != nil {
		return LegacySettingsPlan{}, err
	}
	return loaded.plan, nil
}

// ImportLegacySettings atomically creates a previously absent station data
// directory.  A matching receipt makes a retry idempotent; any other existing
// directory is rejected rather than merged with unknown station state.
func ImportLegacySettings(ctx context.Context, sourceAdminDir, targetDir, expectedDigest string) (LegacySettingsPlan, error) {
	loaded, err := loadLegacySettings(ctx, sourceAdminDir, targetDir)
	if err != nil {
		return LegacySettingsPlan{}, err
	}
	if expectedDigest == "" || expectedDigest != loaded.plan.Digest {
		return LegacySettingsPlan{}, errLegacySettingsChanged
	}
	if err = ctx.Err(); err != nil {
		return LegacySettingsPlan{}, err
	}

	parentPath, targetName := filepath.Dir(loaded.plan.Target), filepath.Base(loaded.plan.Target)
	parent, err := workspacefs.OpenPath(parentPath)
	if err != nil {
		return LegacySettingsPlan{}, errLegacySettingsPath
	}
	defer parent.Close()
	release, err := hostfile.Lock(filepath.Join(parentPath, ".legacy-settings-import.lock"))
	if err != nil {
		return LegacySettingsPlan{}, errors.New("旧设置接续暂不可写")
	}
	defer release()
	if existing, matched, checkErr := existingLegacySettingsImport(parent, targetName, loaded); checkErr != nil {
		return LegacySettingsPlan{}, checkErr
	} else if existing {
		if matched {
			return loaded.plan, nil
		}
		return LegacySettingsPlan{}, errLegacySettingsConflict
	}
	if err = ctx.Err(); err != nil {
		return LegacySettingsPlan{}, err
	}

	stageName, stageRoot, err := makeLegacySettingsStage(parent)
	if err != nil {
		return LegacySettingsPlan{}, errors.New("无法准备旧设置接续")
	}
	defer stageRoot.Close()
	defer func() { _ = parent.RemoveAll(stageName) }()
	modelData, err := json.Marshal(loaded.record)
	if err != nil {
		return LegacySettingsPlan{}, errors.New("无法编码旧模型设置")
	}
	if err = writeLegacySettingsStageFile(stageRoot, "local-model.json", modelData); err != nil {
		return LegacySettingsPlan{}, err
	}
	profiles, err := json.Marshal(localAppPersonaRecord{Version: 1, Apps: map[string]localAppPersona{"wechat": {Revision: 1, Persona: loaded.appPersona}}})
	if err != nil {
		return LegacySettingsPlan{}, err
	}
	if err = writeLegacySettingsStageFile(stageRoot, localAppPersonasFile, profiles); err != nil {
		return LegacySettingsPlan{}, err
	}
	if len(loaded.identity) > 0 {
		if err = writeLegacySettingsStageFile(stageRoot, legacySettingsIdentityFile, loaded.identity); err != nil {
			return LegacySettingsPlan{}, err
		}
	}
	receipt := legacySettingsReceipt{
		Version: 2, Digest: loaded.plan.Digest, Source: loaded.plan.Source,
		ConfigSHA256: loaded.configHash, IdentitySHA256: loaded.idHash,
		Model: loaded.plan.Model, FishID: loaded.plan.FishID,
	}
	receiptData, err := json.Marshal(receipt)
	if err != nil {
		return LegacySettingsPlan{}, errors.New("无法记录旧设置接续")
	}
	if err = writeLegacySettingsStageFile(stageRoot, legacySettingsReceiptFile, receiptData); err != nil {
		return LegacySettingsPlan{}, err
	}
	if err = ctx.Err(); err != nil {
		return LegacySettingsPlan{}, err
	}
	parentInfo, statErr := parent.Lstat(stageName)
	stageInfo, stageErr := stageRoot.Stat(".")
	if statErr != nil || stageErr != nil || !os.SameFile(parentInfo, stageInfo) {
		return LegacySettingsPlan{}, errors.New("旧设置接续暂存目录已变化")
	}
	// The pinned parent root ensures a replaced path cannot redirect publication.
	if err = parent.Rename(stageName, targetName); err != nil {
		// A concurrent winner may have published the exact same preflight.
		if existing, matched, checkErr := existingLegacySettingsImport(parent, targetName, loaded); checkErr == nil && existing && matched {
			return loaded.plan, nil
		}
		return LegacySettingsPlan{}, errors.New("无法启用旧设置接续")
	}
	return loaded.plan, nil
}

func loadLegacySettings(ctx context.Context, sourceAdminDir, targetDir string) (legacySettingsLoaded, error) {
	if err := ctx.Err(); err != nil {
		return legacySettingsLoaded{}, err
	}
	source, err := cleanLegacySettingsPath(sourceAdminDir)
	if err != nil {
		return legacySettingsLoaded{}, errLegacySettingsPath
	}
	target, err := cleanLegacySettingsPath(targetDir)
	if err != nil {
		return legacySettingsLoaded{}, errLegacySettingsPath
	}
	if pathsOverlap(source, target) {
		return legacySettingsLoaded{}, errLegacySettingsPath
	}
	sourceRoot, err := workspacefs.OpenPath(source)
	if err != nil {
		return legacySettingsLoaded{}, errLegacySettingsPath
	}
	defer sourceRoot.Close()
	parent, err := workspacefs.OpenPath(filepath.Dir(target))
	if err != nil {
		return legacySettingsLoaded{}, errLegacySettingsPath
	}
	defer parent.Close()
	if info, statErr := parent.Lstat(filepath.Base(target)); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return legacySettingsLoaded{}, errLegacySettingsPath
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return legacySettingsLoaded{}, errLegacySettingsPath
	}
	config, err := readLegacySettingsFile(sourceRoot, legacySettingsConfigFile, true, legacySettingsSourceLimit)
	if err != nil {
		return legacySettingsLoaded{}, errors.New("无法读取旧管理模型设置")
	}
	defaults := defaultLocalModelSettings()
	character := companion.Defaults()
	old := legacySettingsConfig{
		Provider: defaults.Provider, BaseURL: defaults.BaseURL, Model: defaults.Model,
		MaxTokens: defaults.MaxTokens, ReasoningEffort: defaults.ReasoningEffort,
		SystemPrompt: deepseek.DefaultSystemPrompt,
	}
	// Older management files inherit these defaults when fields are absent.
	// ThinkingEnabled deliberately retains the former default of false.
	old.Companion.Persona = character.Persona
	old.Companion.Style = character.Style
	old.Companion.Relationship = character.Relationship
	if json.Unmarshal(config, &old) != nil {
		return legacySettingsLoaded{}, errors.New("旧管理模型设置无效")
	}
	if old.SystemPrompt == deepseek.LegacySystemPrompt {
		old.SystemPrompt = deepseek.DefaultSystemPrompt
	}
	persona := mergedLegacyPersona(old)
	record := localModelRecord{
		LocalModelSettings: LocalModelSettings{
			Revision: 1, Provider: old.Provider, BaseURL: old.BaseURL, Model: old.Model,
			MaxTokens: old.MaxTokens, ThinkingEnabled: old.ThinkingEnabled,
			ReasoningEffort: old.ReasoningEffort, Persona: defaults.Persona,
		},
		APIKey: old.APIKey,
	}
	record.KeyConfigured = record.APIKey != ""
	if err = validateLocalModelRecord(record); err != nil {
		return legacySettingsLoaded{}, fmt.Errorf("旧管理模型设置不适用于成长站：%w", err)
	}
	if !validAppPersona(localAppPersona{Revision: 1, Persona: persona}) {
		return legacySettingsLoaded{}, errors.New("旧微信角色设定超过应用子角色长度要求")
	}
	if err = ctx.Err(); err != nil {
		return legacySettingsLoaded{}, err
	}
	identityData, err := readLegacySettingsFile(sourceRoot, legacySettingsIdentityFile, false, legacySettingsSourceLimit)
	if err != nil {
		return legacySettingsLoaded{}, errors.New("旧鱼身份不可读")
	}
	var fishID string
	if len(identityData) > 0 {
		fishID, err = validateLegacyIdentity(identityData)
		if err != nil {
			return legacySettingsLoaded{}, errors.New("旧鱼身份无效")
		}
	}
	configHash := legacySettingsHash(config)
	idHash := legacySettingsHash(identityData)
	view := LegacyModelSettingsView{
		Provider: record.Provider, BaseURL: record.BaseURL, Model: record.Model,
		MaxTokens: record.MaxTokens, ThinkingEnabled: record.ThinkingEnabled,
		ReasoningEffort: record.ReasoningEffort, KeyConfigured: record.APIKey != "",
		PersonaSet:       strings.TrimSpace(record.Persona) != "",
		WechatPersonaSet: persona != "",
	}
	digest := settingsDigest(source, target, configHash, idHash)
	return legacySettingsLoaded{plan: LegacySettingsPlan{Digest: digest, Source: source, Target: target, Model: view, HasIdentity: len(identityData) > 0, FishID: fishID}, record: record, identity: identityData, configHash: configHash, idHash: idHash, appPersona: persona}, nil
}

func readLegacySettingsFile(root *os.Root, name string, required bool, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("invalid legacy settings file")
	}
	file, err := workspacefs.OpenRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("invalid legacy settings file")
	}
	return data, nil
}

// Registry.Read creates a lock beside its path.  Validate the copied record
// in a controlled temporary directory so merely previewing legacy data never
// changes the stopped source installation.
func validateLegacyIdentity(data []byte) (string, error) {
	dir, err := os.MkdirTemp("", "station-legacy-fish-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err = hostfile.WritePrivate(filepath.Join(dir, legacySettingsIdentityFile), data); err != nil {
		return "", err
	}
	record, err := fishidentity.New(dir).Read()
	if err != nil || !fishidentity.ValidFishID(record.FishID) {
		return "", errors.New("invalid fish identity")
	}
	return record.FishID, nil
}

func existingLegacySettingsImport(parent *os.Root, targetName string, loaded legacySettingsLoaded) (exists, matched bool, err error) {
	info, statErr := parent.Lstat(targetName)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, false, nil
	}
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return true, false, errLegacySettingsConflict
	}
	target, openErr := workspacefs.OpenExistingDirectory(parent, targetName)
	if openErr != nil {
		return true, false, errLegacySettingsConflict
	}
	defer target.Close()
	receiptData, receiptErr := readLegacySettingsFile(target, legacySettingsReceiptFile, true, legacySettingsTargetLimit)
	if receiptErr != nil {
		return true, false, nil
	}
	var receipt legacySettingsReceipt
	if json.Unmarshal(receiptData, &receipt) != nil || receipt.Version != 2 || receipt.Digest != loaded.plan.Digest || receipt.Source != loaded.plan.Source || receipt.ConfigSHA256 != loaded.configHash || receipt.IdentitySHA256 != loaded.idHash {
		return true, false, nil
	}
	modelData, modelErr := readLegacySettingsFile(target, "local-model.json", true, legacySettingsTargetLimit)
	if modelErr != nil {
		return true, false, nil
	}
	var record localModelRecord
	if json.Unmarshal(modelData, &record) != nil || validateLocalModelRecord(record) != nil { // validate exact target state below
		return true, false, nil
	}
	if record != loaded.record {
		return true, false, nil
	}
	profile, profileErr := (localAppPersonaStore{dir: loaded.plan.Target}).View("wechat")
	if profileErr != nil || profile.Revision != 1 || profile.Persona != loaded.appPersona {
		return true, false, nil
	}
	if len(loaded.identity) > 0 {
		data, identityErr := readLegacySettingsFile(target, legacySettingsIdentityFile, true, legacySettingsSourceLimit)
		if identityErr != nil || legacySettingsHash(data) != loaded.idHash {
			return true, false, nil
		}
	} else if _, statErr = target.Lstat(legacySettingsIdentityFile); !errors.Is(statErr, os.ErrNotExist) {
		return true, false, nil
	}
	return true, true, nil
}

func cleanLegacySettingsPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if filepath.Base(abs) == "." || filepath.Dir(abs) == abs {
		return "", errors.New("invalid target")
	}
	return abs, nil
}

func makeLegacySettingsStage(parent *os.Root) (string, *os.Root, error) {
	for range 8 {
		var token [12]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", nil, err
		}
		name := ".legacy-settings-" + hex.EncodeToString(token[:])
		if err := parent.Mkdir(name, 0700); err == nil {
			// Re-open via the pinned parent before writing by its absolute path.
			stage, openErr := workspacefs.OpenExistingDirectory(parent, name)
			if openErr != nil {
				_ = parent.RemoveAll(name)
				return "", nil, openErr
			}
			return name, stage, nil
		} else if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("无法创建旧设置接续暂存目录")
}

func writeLegacySettingsStageFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("无法写入旧设置接续")
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		_ = root.Remove(name)
		return errors.New("无法写入旧设置接续")
	}
	return nil
}

func pathsOverlap(first, second string) bool {
	for _, pair := range [][2]string{{first, second}, {second, first}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

func settingsDigest(source, target, configHash, identityHash string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"legacy-settings-v1", source, target, configHash, identityHash}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func legacySettingsHash(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

// mergedLegacyPersona carries every supported character-setting field into the
// WeChat app persona without pretending that reply scopes or other legacy
// behavior options were migrated. Companion persona remains the primary text;
// the other user-authored text is labelled and omitted only when identical.
func mergedLegacyPersona(old legacySettingsConfig) string {
	primary := strings.TrimSpace(old.Companion.Persona)
	if primary == "" {
		primary = strings.TrimSpace(old.SystemPrompt)
	}
	parts := []string{primary}
	seen := map[string]bool{primary: true}
	appendDistinct := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		parts = append(parts, label+"："+value)
	}
	if strings.TrimSpace(old.Companion.Persona) != "" {
		appendDistinct("旧系统提示", old.SystemPrompt)
	}
	appendDistinct("相处风格", old.Companion.Style)
	appendDistinct("关系设定", old.Companion.Relationship)
	return strings.Join(parts, "\n\n")
}
