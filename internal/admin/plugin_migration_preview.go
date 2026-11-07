package admin

// This file builds a read-only migration proposal.  It deliberately has no
// package-store call: opening the store can create its database, while a
// preview must not alter production state.  The transactional applier receives
// the unexported ConfigUpdate values only after it rechecks the public hash.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	legacyplugins "wechat-bot/internal/plugins"
	"wechat-bot/internal/sharespec"
	catalog "wechat-bot/plugins"
)

const (
	pluginMigrationPreviewVersion = 1
	// legacyAudioReviewModel is an immutable record of the legacy
	// ReviewAudio route, not a default for new media plugin configurations.
	legacyAudioReviewModel = "qwen3.8-omni-flash"
)

type pluginMigrationSecret struct {
	SourceField string `json:"source_field"`
	Present     bool   `json:"present"`
	SHA256      string `json:"sha256,omitempty"`
}
type pluginMigrationPackage struct {
	Version        string   `json:"version,omitempty"`
	CatalogSHA256  string   `json:"catalog_contract_sha256,omitempty"`
	Status         string   `json:"status,omitempty"`
	ExpectedDigest string   `json:"expected_package_digest,omitempty"`
	Permissions    []string `json:"permissions,omitempty"`
	deepSeekSearch bool
	objectBridge   bool
	namedSpeech    bool
	localMusic     bool
	externalSync   bool
	externalJob    bool
	externalPoll   bool
	externalMCP    bool
}
type pluginMigrationResourceProof struct {
	Source        string `json:"source"`
	Present       bool   `json:"present"`
	VersionSHA256 string `json:"version_sha256,omitempty"`
}
type pluginMigrationStep struct {
	Plugin           string                           `json:"plugin"`
	Package          pluginMigrationPackage           `json:"package"`
	Permissions      []string                         `json:"permissions"`
	ExpectedRevision int                              `json:"expected_config_revision"`
	LegacyEnabled    bool                             `json:"legacy_enabled"`
	ProposedEnabled  bool                             `json:"proposed_enabled"`
	EligibleToEnable bool                             `json:"eligible_to_enable_after_confirmation"`
	Status           string                           `json:"status"`
	Values           map[string]json.RawMessage       `json:"values"`
	Secrets          map[string]pluginMigrationSecret `json:"secrets"`
	RequiredMissing  []string                         `json:"required_missing"`
	UnmappedLegacy   []string                         `json:"unmapped_legacy_fields,omitempty"`
	ProfileID        string                           `json:"profile_id,omitempty"`
	ResourceProof    *pluginMigrationResourceProof    `json:"resource_proof,omitempty"`
	ApplySourceHash  string                           `json:"apply_source_hash"`
	// update is host-only. It is never serialized through the management API.
	update pluginhost.ConfigUpdate
}
type pluginMigrationAccount struct {
	Account      string                `json:"account"`
	SourceHashes map[string]string     `json:"source_hashes"`
	SourceErrors map[string]string     `json:"source_errors,omitempty"`
	Plugins      []pluginMigrationStep `json:"plugins"`
}
type pluginMigrationPreview struct {
	SchemaVersion int                      `json:"schema_version"`
	Kind          string                   `json:"kind"`
	ReadOnly      bool                     `json:"read_only"`
	Accounts      []pluginMigrationAccount `json:"accounts"`
}

func migrationSourceHash(path string) (string, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		marker := "missing"
		if !errors.Is(err, os.ErrNotExist) {
			marker = "unreadable"
		}
		sum := sha256.Sum256([]byte(marker))
		return hex.EncodeToString(sum[:]), marker
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), ""
}

func migrationBytesHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func migrationSecret(value, source string) (pluginMigrationSecret, string) {
	out := pluginMigrationSecret{SourceField: source, Present: value != ""}
	if value != "" {
		sum := sha256.Sum256([]byte(value))
		out.SHA256 = hex.EncodeToString(sum[:])
	}
	return out, value
}
func migrationValue(value any) json.RawMessage { b, _ := json.Marshal(value); return b }

func migrationEndpoint(root, suffix string) string {
	root = strings.TrimRight(root, "/")
	if root == "" {
		return ""
	}
	return root + suffix
}

func migrationCatalog() map[string]pluginMigrationPackage {
	items, err := catalog.List()
	if err != nil {
		return map[string]pluginMigrationPackage{}
	}
	out := make(map[string]pluginMigrationPackage, len(items))
	for _, item := range items {
		contract, err := catalog.ContractFor(item.ID)
		if err != nil {
			continue
		}
		out[item.ID] = pluginMigrationPackage{
			Version:        contract.Package.Version,
			CatalogSHA256:  contract.ManifestSHA256,
			Status:         contract.Package.Status,
			ExpectedDigest: contract.ManifestSHA256,
			Permissions:    append([]string(nil), contract.Package.Permissions...),
			deepSeekSearch: catalogDeclaresDeepSeekSearch(contract),
			objectBridge:   catalogDeclaresShareObjectBridge(contract),
			namedSpeech:    catalogDeclaresNamedSpeech(contract),
			localMusic:     catalogDeclaresLocalMusicBridge(contract),
			externalSync:   catalogDeclaresExternalSync(contract),
			externalJob:    catalogDeclaresExternalJob(contract),
			externalPoll:   catalogDeclaresExternalPoll(contract),
			externalMCP:    catalogDeclaresExternalMCP(contract),
		}
	}
	return out
}

// External tools remain host-owned declarations.  The first migration slice
// only installs the reviewed synchronous HTTP adapter; it does not copy an
// endpoint, credentials, MCP server, job or polling protocol into plugin
// configuration.  Those details stay in plugins.json and are rechecked by the
// host bridge for each call.
func catalogDeclaresExternalSync(contract catalog.Contract) bool {
	return catalogDeclaresExternalCapability(contract, externalHTTPCapability)
}

// Job requests are a separately reviewed capability.  A ready one-shot HTTP
// capability never implies that the package understands persisted start/poll/
// cancel tickets.
func catalogDeclaresExternalJob(contract catalog.Contract) bool {
	return catalogDeclaresExternalCapability(contract, externalJobCapability)
}

func catalogDeclaresExternalPoll(contract catalog.Contract) bool {
	return catalogDeclaresExternalCapability(contract, externalPollCapability)
}

func catalogDeclaresExternalMCP(contract catalog.Contract) bool {
	return catalogDeclaresExternalCapability(contract, externalMCPCapability)
}

func catalogDeclaresExternalCapability(contract catalog.Contract, wanted string) bool {
	p := contract.Package
	if p.ID != externalHTTPPluginID || p.Status != "ready" || !migrationStringContains(p.Permissions, "network") {
		return false
	}
	if contract.Configuration.Type != "object" || contract.Configuration.AdditionalProperties || len(contract.Configuration.Properties) != 0 {
		return false
	}
	for _, capability := range contract.Capabilities {
		if capability.ID == wanted && capability.Implementation == "ready" && capability.Discoverable && !capability.Async && migrationStringContains(capability.Permissions, "network") {
			return true
		}
	}
	return false
}

type legacyExternalIntent struct {
	declared         bool
	synchronous      bool
	jobs             bool
	polls            bool
	mcp              bool
	rawStdio         bool
	unsupported      bool
	fromMCPConfig    bool
	fromPluginLedger bool
}

// legacyExternalIntentFor deliberately examines only enablement and lifecycle
// shape. It neither serializes nor interprets URLs, templates, headers or
// Secrets: plugins.json remains their sole authority. HTTP, Job+Poll,
// Poll-without-Job and declared remote MCP have distinct reviewed adapters.
// Raw stdio command declarations are deliberately not execution authority.
func legacyExternalIntentFor(mcp mcpSettings, installed []legacyplugins.Installed) legacyExternalIntent {
	var out legacyExternalIntent
	if !mcp.Enabled {
		return out
	}
	for _, server := range mcp.Servers {
		if server.Enabled {
			out.fromMCPConfig = true
			out.addMCP(server)
		}
	}
	for _, item := range installed {
		if !item.Enabled {
			continue
		}
		if item.Manifest.MCP != nil {
			out.fromPluginLedger = true
			out.addMCP(*item.Manifest.MCP)
		}
		for _, tool := range item.Manifest.Tools {
			out.declared = true
			out.fromPluginLedger = true
			switch {
			case tool.Job == nil && tool.Poll == nil:
				out.synchronous = true
			case tool.Job != nil && tool.Poll != nil:
				out.jobs = true
			case tool.Job == nil && tool.Poll != nil:
				out.polls = true
			default:
				out.unsupported = true
			}
		}
	}
	return out
}

func (out *legacyExternalIntent) addMCP(server mcptools.ServerConfig) {
	out.declared = true
	server = server.Normalized()
	switch server.Transport {
	case "http", "streamable-http":
		out.mcp = true
	case "stdio":
		if server.PluginID != "" && server.Command == "" && len(server.Args) == 0 && len(server.Env) == 0 {
			out.mcp = true
		} else {
			out.rawStdio = true
		}
	default:
		out.unsupported = true
	}
}

// A YuE/RVC endpoint is not a generic plugin network permission.  The music
// package must explicitly own both host-only fields and the reviewed bridge
// capability before a legacy local token can be prepared for migration.
func catalogDeclaresLocalMusicBridge(contract catalog.Contract) bool {
	p := contract.Package
	if p.ID != "music" || p.Status != "ready" || !migrationStringContains(p.Permissions, "network") {
		return false
	}
	endpoint, endpointOK := contract.Configuration.Properties["local_endpoint"]
	token, tokenOK := contract.Configuration.Properties["local_token"]
	if !endpointOK || endpoint.Type != "string" || endpoint.Secret || endpoint.HostOnly || !tokenOK || token.Type != "string" || !token.Secret || !token.HostOnly {
		return false
	}
	for _, capability := range contract.Capabilities {
		if capability.ID == localMusicCapability && capability.Implementation == "ready" && migrationStringContains(capability.Permissions, "network") {
			return true
		}
	}
	return false
}

// Named speech requires an explicit local profile field and the ready named
// capability.  A cloud TTS package must never acquire a local profile merely
// because a legacy voice happens to have a familiar display name.
func catalogDeclaresNamedSpeech(contract catalog.Contract) bool {
	p := contract.Package
	if p.ID != "speech" || p.Status != "ready" || !migrationStringContains(p.Permissions, "artifact.read") || !migrationStringContains(p.Permissions, "model.invoke") {
		return false
	}
	profile, ok := contract.Configuration.Properties["local_profile_id"]
	if !ok || profile.Type != "string" || !migrationEnumContains(profile.Enum, "gpt-sovits") {
		return false
	}
	for _, capability := range contract.Capabilities {
		if capability.ID == "speech.synthesize_named" && capability.Implementation == "ready" && migrationStringContains(capability.Permissions, "artifact.read") && migrationStringContains(capability.Permissions, "model.invoke") {
			return true
		}
	}
	return false
}

// The fixed host route, explicit backend schema, host-only secret and ready
// capability must all be declared before a preview can map a legacy paid key.
// This prevents an arbitrary network package from receiving the key and also
// prevents a silent fallback to a public provider.
func catalogDeclaresDeepSeekSearch(contract catalog.Contract) bool {
	p := contract.Package
	if p.ID != "web-search" || p.Status != "ready" {
		return false
	}
	backend, backendOK := contract.Configuration.Properties["search_backend"]
	key, keyOK := contract.Configuration.Properties["search_key"]
	if !backendOK || backend.Type != "string" || !migrationEnumContains(backend.Enum, "deepseek") || !keyOK || key.Type != "string" || !key.Secret || !key.HostOnly {
		return false
	}
	capabilityOK := false
	for _, capability := range contract.Capabilities {
		if capability.ID == "web.search" && capability.Implementation == "ready" && migrationStringContains(capability.Permissions, "network") {
			capabilityOK = true
			break
		}
	}
	if !capabilityOK {
		return false
	}
	for _, route := range p.HTTPRoutes {
		if route.ID == "deepseek-search" && route.Endpoint == "https://api.deepseek.com/v1/tools/search" && route.Permission == "network" && route.Credential == "search_key" && route.CredentialHeader == "Authorization" && route.CredentialPrefix == "Bearer " && len(route.Capabilities) == 1 && route.Capabilities[0] == "web.search" {
			return true
		}
	}
	return false
}

// Object bridges are host-owned loopback configuration. A migration may copy
// one only when the share-reader's reviewed configuration schema owns this
// ordinary setting. It must never smuggle a bridge URL into another plugin.
func catalogDeclaresShareObjectBridge(contract catalog.Contract) bool {
	if contract.Package.ID != "share-reader" {
		return false
	}
	field, ok := contract.Configuration.Properties["object_bridge_url"]
	return ok && field.Type == "string" && !field.Secret && !field.HostOnly
}

func migrationEnumContains(values []json.RawMessage, wanted string) bool {
	for _, value := range values {
		var item string
		if json.Unmarshal(value, &item) == nil && item == wanted {
			return true
		}
	}
	return false
}

func migrationStringContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func migrationStep(id string, packages map[string]pluginMigrationPackage, legacyEnabled bool, values map[string]json.RawMessage, secrets map[string]pluginMigrationSecret, update pluginhost.ConfigUpdate, required, unmapped []string, profile string, sources map[string]string) pluginMigrationStep {
	pkg := packages[id]
	if pkg.ExpectedDigest == "" {
		required = append(required, "official_package_digest_unavailable")
	}
	if _, err := officialMigrationDirectory(id); err != nil {
		required = append(required, "official_package_source_unavailable")
	}
	if pkg.Status != "ready" {
		required = append(required, "plugin_package_not_ready")
	}
	sort.Strings(required)
	required = uniqueMigrationStrings(required)
	sort.Strings(unmapped)
	unmapped = uniqueMigrationStrings(unmapped)
	step := pluginMigrationStep{Plugin: id, Package: pkg, Permissions: append([]string(nil), pkg.Permissions...), ExpectedRevision: 0, LegacyEnabled: legacyEnabled, ProposedEnabled: false, Values: values, Secrets: secrets, RequiredMissing: required, UnmappedLegacy: unmapped, ProfileID: profile, update: update}
	if len(required) == 0 {
		step.Status = "ready_for_administrator_review"
		step.EligibleToEnable = legacyEnabled
	} else {
		step.Status = "blocked"
	}
	canonical := struct {
		Plugin   string                           `json:"plugin"`
		Package  pluginMigrationPackage           `json:"package"`
		Revision int                              `json:"revision"`
		Sources  map[string]string                `json:"sources"`
		Values   map[string]json.RawMessage       `json:"values"`
		Secrets  map[string]pluginMigrationSecret `json:"secrets"`
	}{id, pkg, step.ExpectedRevision, sources, values, secrets}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	step.ApplySourceHash = hex.EncodeToString(sum[:])
	return step
}
func uniqueMigrationStrings(in []string) []string {
	out := in[:0]
	for _, value := range in {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func officialDeepSeekMigration(c Config) bool {
	base := strings.TrimRight(c.BaseURL, "/")
	return c.Provider == "deepseek" && c.APIKey != "" && (base == "https://api.deepseek.com" || base == "https://api.deepseek.com/v1")
}

// Resolve only the existing public registration under the trusted official
// package/grant contract. A migration preview must not open the package Store,
// change enablement, provision resources or start an interpreter.
func (m *Manager) migrationLocalASRResourceVersion(ctx context.Context, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	directory, err := officialMigrationDirectory(localasr.PluginID)
	if err != nil {
		return "", err
	}
	pkg, err := pluginhost.Verify(directory)
	if err != nil || pkg.ID != localasr.PluginID {
		return "", errors.New("local_asr_migration_package_unavailable")
	}
	grant := pluginhost.Grant{Account: m.native.Account(), Plugin: pkg.ID, Version: pkg.Version, Permissions: append([]string(nil), pkg.Permissions...)}
	invocation := pluginhost.Invocation{Account: grant.Account, Thread: "host:plugin-migration-preview", Requester: "administrator", Purpose: "inspect registered local ASR migration resources", RequestKey: "local-asr-migration-preview"}
	if err := pluginhost.Authorize(pkg, grant, invocation, pkg.Permissions); err != nil {
		return "", err
	}
	profile, err := m.readRegisteredLocalASRProfile(pkg, grant, id)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if profile.Metadata().ProfileID != id || profile.Metadata().ResourceVersion == "" {
		return "", errors.New("local_asr_migration_resource_unavailable")
	}
	return profile.Metadata().ResourceVersion, nil
}

// migrationNamedSpeechResourceVersion verifies only the existing public
// GPT-SoVITS registration under the official speech package grant.  It does
// not open the plugin Store, enumerate named voices, copy weights, or start a
// local interpreter.  The returned digest is deliberately opaque.
func (m *Manager) migrationNamedSpeechResourceVersion(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	directory, err := officialMigrationDirectory("speech")
	if err != nil {
		return "", err
	}
	pkg, err := pluginhost.Verify(directory)
	if err != nil || pkg.ID != "speech" {
		return "", errors.New("named_speech_migration_package_unavailable")
	}
	grant := pluginhost.Grant{Account: m.native.Account(), Plugin: pkg.ID, Version: pkg.Version, Permissions: append([]string(nil), pkg.Permissions...)}
	invocation := pluginhost.Invocation{Account: grant.Account, Thread: "host:plugin-migration-preview", Requester: "administrator", Purpose: "inspect registered named speech migration resources", RequestKey: "named-speech-migration-preview"}
	if err := pluginhost.Authorize(pkg, grant, invocation, []string{"artifact.read", "model.invoke"}); err != nil {
		return "", err
	}
	version, ready, err := namedSpeechManifestResourceProof(ctx, filepath.Join(m.managedRoot(), "public_workspace"))
	if err != nil {
		return "", err
	}
	if !ready || version == "" {
		return "", errors.New("named_speech_migration_resource_unavailable")
	}
	return version, nil
}

func (m *Manager) pluginMigrationPreviewFor(ctx context.Context, packages map[string]pluginMigrationPackage) (pluginMigrationAccount, error) {
	if err := ctx.Err(); err != nil {
		return pluginMigrationAccount{}, err
	}
	paths := map[string]string{"config_json": filepath.Join(m.dir, "config.json"), "speech_config_json": filepath.Join(m.dir, "speech-config.json"), "mcp_config_json": filepath.Join(m.dir, "mcp-config.json"), "channels_config_json": filepath.Join(m.dir, "channels-config.json"), "agent_config_json": filepath.Join(m.dir, "agent-config.json"), "plugins_json": filepath.Join(m.dir, "plugins.json")}
	activeExternal, externalActive := m.externalConfigurationSources()
	externalSourceName := map[string]string{"mcp_config_json": "mcp-config.json", "plugins_json": "plugins.json"}
	sourceHash := func(name, path string, sources map[string][]byte, active bool) (string, string) {
		if active {
			if source, ok := externalSourceName[name]; ok {
				return migrationBytesHash(sources[source]), ""
			}
		}
		return migrationSourceHash(path)
	}
	before := map[string]string{}
	for name, path := range paths {
		before[name], _ = sourceHash(name, path, activeExternal, externalActive)
	}
	m.mu.Lock()
	config, mcp := cloneConfig(m.config), cloneMCP(m.mcp)
	m.mu.Unlock()
	speech, speechSourceHash, speechErr := loadSpeechMigrationSource(m.dir)
	if speechErr != nil {
		return pluginMigrationAccount{}, speechErr
	}
	if before["speech_config_json"] != speechSourceHash {
		return pluginMigrationAccount{}, errors.New("migration source changed during preview")
	}
	agent := m.agent.config()
	channelSettings, channelsErr := loadChannelsSettings(m.dir)
	legacyExternal := m.plugins.installed()
	if externalActive {
		var activeMCP mcpSettings
		var activeEntries map[string]pluginEntry
		if json.Unmarshal(activeExternal["mcp-config.json"], &activeMCP) != nil || json.Unmarshal(activeExternal["plugins.json"], &activeEntries) != nil {
			return pluginMigrationAccount{}, errExternalConfigurationCorrupt
		}
		mcp = cloneMCP(activeMCP)
		legacyExternal = []legacyplugins.Installed{}
		for _, entry := range activeEntries {
			if !entry.Removed {
				legacyExternal = append(legacyExternal, clonePluginEntry(entry).Installed)
			}
		}
		sort.Slice(legacyExternal, func(i, j int) bool { return legacyExternal[i].Manifest.ID < legacyExternal[j].Manifest.ID })
	}
	sources := map[string]string{}
	sourceErrors := map[string]string{}
	activeAfter, activeAfterEnabled := m.externalConfigurationSources()
	if externalActive != activeAfterEnabled {
		return pluginMigrationAccount{}, errors.New("migration source changed during preview")
	}
	for name, path := range paths {
		hash, problem := sourceHash(name, path, activeAfter, activeAfterEnabled)
		sources[name] = hash
		if problem != "" {
			sourceErrors[name] = problem
		}
	}
	for name, hash := range sources {
		if before[name] != hash {
			return pluginMigrationAccount{}, errors.New("migration source changed during preview")
		}
	}
	if channelsErr != nil {
		sourceErrors["channels_config_json"] = "invalid"
	}

	visionSecret, visionKey := migrationSecret(config.APIKey, "config.json:api_key")
	asrSecret, speechKey := migrationSecret(speech.Key, "speech-config.json:key")
	mediaValues := map[string]json.RawMessage{"vision_endpoint": migrationValue(migrationEndpoint(config.BaseURL, "/chat/completions")), "vision_model": migrationValue(config.Model), "vision_backend": migrationValue(config.Provider), "max_tokens": migrationValue(config.MaxTokens), "thinking": migrationValue(config.ThinkingEnabled), "reasoning_effort": migrationValue(config.ReasoningEffort)}
	mediaUpdate := pluginhost.ConfigUpdate{Values: mediaValues, Secrets: map[string]string{"vision_key": visionKey}}
	mediaRequired := []string{}
	visionEnabled := config.APIKey != "" && config.Model != ""
	if visionEnabled && visionKey == "" {
		mediaRequired = append(mediaRequired, "vision_key")
	}
	asrEnabled := speech.ASREnabled
	backend := speech.ASRBackend
	if backend == "" {
		backend = "bailian"
	}
	profile := ""
	if asrEnabled {
		switch backend {
		case "local":
			mediaValues["asr_backend"] = migrationValue("local")
			profile = speech.ASRLocalModel
			if profile == "" {
				profile = "sensevoice-small-int8"
			}
			mediaValues["local_asr_profile_id"] = migrationValue(profile)
			resourceVersion, resourceErr := m.migrationLocalASRResourceVersion(ctx, profile)
			if err := ctx.Err(); err != nil {
				return pluginMigrationAccount{}, err
			}
			if resourceErr != nil {
				mediaRequired = append(mediaRequired, "registered_local_asr_profile:"+profile)
			} else {
				sources["local_asr_profile_resource_version"] = resourceVersion
			}
		case "bailian":
			mediaValues["asr_backend"] = migrationValue("remote")
			mediaValues["asr_endpoint"] = migrationValue(migrationEndpoint(speech.Endpoint, "/api/v1/services/aigc/multimodal-generation/generation"))
			mediaValues["asr_model"] = migrationValue(speech.ASRModel)
			mediaUpdate.Secrets["asr_key"] = speechKey
			if speechKey == "" {
				mediaRequired = append(mediaRequired, "asr_key")
			}
		case "local_then_bailian":
			mediaRequired = append(mediaRequired, "asr_backend:legacy_local_then_bailian_requires_explicit_choice")
		default:
			mediaRequired = append(mediaRequired, "asr_backend:unsupported_legacy_value")
		}
	}
	mediaSecrets := map[string]pluginMigrationSecret{"vision_key": visionSecret, "asr_key": asrSecret}
	// Legacy ReviewAudio always used the configured DashScope regional root and
	// the fixed Omni Flash model. Keep it separate from vision/ASR: possessing a
	// legacy key must not make the media plugin eligible to auto-enable.
	if speechKey != "" {
		audioReviewSecret, audioReviewKey := migrationSecret(speech.Key, "speech-config.json:key")
		mediaValues["audio_review_endpoint"] = migrationValue(migrationEndpoint(speech.Endpoint, "/compatible-mode/v1/chat/completions"))
		mediaValues["audio_review_model"] = migrationValue(legacyAudioReviewModel)
		mediaUpdate.Secrets["audio_review_key"] = audioReviewKey
		mediaSecrets["audio_review_key"] = audioReviewSecret
	}
	media := migrationStep("media-understanding", packages, visionEnabled || asrEnabled, mediaValues, mediaSecrets, mediaUpdate, mediaRequired, nil, profile, sources)

	ttsSecret, ttsKey := migrationSecret(speech.Key, "speech-config.json:key")
	speechValues := map[string]json.RawMessage{"tts_endpoint": migrationValue(migrationEndpoint(speech.Endpoint, "/api/v1/services/audio/tts/SpeechSynthesizer")), "tts_model": migrationValue(speech.TTSModel), "default_voice": migrationValue(speech.Voice), "default_instruction": migrationValue(speech.Instruction)}
	speechRequired := []string{}
	if speech.TTSEnabled && ttsKey == "" {
		speechRequired = append(speechRequired, "tts_key")
	}
	speechUpdate := pluginhost.ConfigUpdate{Values: speechValues, Secrets: map[string]string{"tts_key": ttsKey}}
	var namedProof *pluginMigrationResourceProof
	namedAvailable := false
	if version, resourceErr := m.migrationNamedSpeechResourceVersion(ctx); resourceErr == nil {
		if packages["speech"].namedSpeech {
			speechValues["local_profile_id"] = migrationValue("gpt-sovits")
			speechUpdate.Values["local_profile_id"] = migrationValue("gpt-sovits")
			namedProof = &pluginMigrationResourceProof{Source: "gpt-sovits-manifest", Present: true, VersionSHA256: version}
			sources["named_speech_resource_version"] = version
			namedAvailable = true
		} else {
			// Do not silently drop a verified local route when the selected
			// package has not declared a compatible configuration/capability.
			speechRequired = append(speechRequired, "official_named_speech_adapter_not_declared_by_speech_plugin")
		}
	} else if err := ctx.Err(); err != nil {
		return pluginMigrationAccount{}, err
	}
	speechStep := migrationStep("speech", packages, speech.TTSEnabled || namedAvailable, speechValues, map[string]pluginMigrationSecret{"tts_key": ttsSecret}, speechUpdate, speechRequired, []string{"asr_*", "music_*", "yue_*", "yue_token"}, "", sources)
	speechStep.ResourceProof = namedProof
	musicSecret, musicKey := migrationSecret(speech.Key, "speech-config.json:key")
	localMusicSecret, localMusicToken := migrationSecret(speech.YuEToken, "speech-config.json:yue_token")
	musicValues := map[string]json.RawMessage{"music_endpoint": migrationValue(migrationEndpoint(speech.Endpoint, "/api/v1/services/audio/music/generation")), "music_model": migrationValue(speech.MusicModel)}
	musicSecrets := map[string]pluginMigrationSecret{"music_key": musicSecret, "local_token": localMusicSecret}
	musicUpdate := pluginhost.ConfigUpdate{Values: musicValues, Secrets: map[string]string{"music_key": musicKey}}
	musicRequired := []string{}
	localMusicConfigured := speech.YuEEnabled || speech.YuEEndpoint != "" || speech.YuEToken != ""
	if speech.MusicEnabled && musicKey == "" && !speech.YuEEnabled {
		musicRequired = append(musicRequired, "music_key")
	}
	if localMusicConfigured {
		switch {
		case !packages["music"].localMusic:
			musicRequired = append(musicRequired, "official_local_music_bridge_not_declared_by_music_plugin")
		case speech.YuEEndpoint == "":
			musicRequired = append(musicRequired, "local_endpoint")
		case !localMusicEndpoint(speech.YuEEndpoint):
			musicRequired = append(musicRequired, "local_endpoint_invalid")
		default:
			musicValues["local_endpoint"] = migrationValue(strings.TrimRight(speech.YuEEndpoint, "/"))
			musicUpdate.Values["local_endpoint"] = musicValues["local_endpoint"]
			if localMusicToken != "" {
				musicUpdate.Secrets["local_token"] = localMusicToken
			}
			if speech.YuEEnabled && localMusicToken == "" {
				musicRequired = append(musicRequired, "local_token")
			}
		}
	}
	musicStep := migrationStep("music", packages, speech.MusicEnabled || speech.YuEEnabled, musicValues, musicSecrets, musicUpdate, musicRequired, []string{"tts_*", "asr_*", "named_voice_library", "rvc_profiles"}, "", sources)

	webRequired, webUnmapped := []string{}, []string{}
	// Keep the legacy switches in the read-only proposal so an administrator can
	// audit the source state.  They are host authorization settings, however,
	// and are not fields declared by the web-search package.  In particular,
	// they must never reach ConfigUpdate.Values: the package schema deliberately
	// rejects undeclared fields.
	webValues := map[string]json.RawMessage{"legacy_web_search_enabled": migrationValue(mcp.WebSearchEnabled), "legacy_web_fetch_enabled": migrationValue(mcp.WebFetchEnabled), "search_backend": migrationValue("public")}
	webSecrets := map[string]pluginMigrationSecret{}
	webUpdateValues := map[string]json.RawMessage{"search_backend": migrationValue("public")}
	webUpdate := pluginhost.ConfigUpdate{Values: webUpdateValues}
	if officialDeepSeekMigration(config) {
		if packages["web-search"].deepSeekSearch {
			webValues["search_backend"] = migrationValue("deepseek")
			webUpdateValues["search_backend"] = migrationValue("deepseek")
			webSecret, searchKey := migrationSecret(config.APIKey, "config.json:api_key")
			webSecrets["search_key"] = webSecret
			webUpdate.Secrets = map[string]string{"search_key": searchKey}
		} else {
			webRequired = append(webRequired, "official_deepseek_search_adapter_not_declared_by_web-search_plugin")
			webUnmapped = append(webUnmapped, "config.json:base_url", "config.json:api_key", "config.json:provider")
		}
	}
	webEnabled := mcp.Enabled && (mcp.WebSearchEnabled || mcp.WebFetchEnabled)
	web := migrationStep("web-search", packages, webEnabled, webValues, webSecrets, webUpdate, webRequired, webUnmapped, "", sources)

	cookieSecret, cookie := migrationSecret(channelSettings.Cookie, "channels-config.json:cookie")
	assistantSecret, assistant := migrationSecret(channelSettings.AssistantCookie, "channels-config.json:assistant_cookie")
	shareRequired := []string{}
	shareValues := map[string]json.RawMessage{}
	if channelSettings.Enabled && cookie == "" && assistant == "" {
		shareRequired = append(shareRequired, "yuanbao_cookie_or_assistant_cookie")
	}
	shareUnmapped := []string{}
	if channelSettings.ObjectBridge != "" {
		bridge, bridgeErr := sharespec.ObjectBridgeURL(channelSettings.ObjectBridge)
		switch {
		case bridgeErr != nil:
			shareRequired = append(shareRequired, "object_bridge_url_invalid")
			shareUnmapped = append(shareUnmapped, "channels-config.json:object_bridge_url")
		case !packages["share-reader"].objectBridge:
			shareRequired = append(shareRequired, "object_bridge_url_not_declared_by_share_reader_plugin")
			shareUnmapped = append(shareUnmapped, "channels-config.json:object_bridge_url")
		default:
			shareValues["object_bridge_url"] = migrationValue(bridge)
			// Legacy handling allowed bridge failure to fall through to a cookie
			// resolver. The plugin contract is stricter, so an administrator
			// must explicitly choose the new policy instead of changing it here.
			if cookie != "" {
				shareRequired = append(shareRequired, "explicit_bridge_fallback_policy_needed")
			}
		}
	}
	share := migrationStep("share-reader", packages, channelSettings.Enabled, shareValues, map[string]pluginMigrationSecret{"yuanbao_cookie": cookieSecret, "assistant_cookie": assistantSecret}, pluginhost.ConfigUpdate{Values: shareValues, Secrets: map[string]string{"yuanbao_cookie": cookie, "assistant_cookie": assistant}}, shareRequired, shareUnmapped, "", sources)

	// Legacy filesystem roots and host grants are security boundaries owned
	// by the host. They are never copied into this empty plugin configuration.
	// Only an explicitly enabled full executor expresses the old intent for all
	// three proxy permissions; read/files require a new administrator decision.
	projectRequired := []string{}
	projectEnabled := agent.Enabled && agent.Host.Mode == "full"
	if !projectEnabled {
		projectRequired = append(projectRequired, "explicit_project_permissions_review_required")
	}
	projectValues := map[string]json.RawMessage{}
	project := migrationStep(projectPluginID, packages, projectEnabled, projectValues, map[string]pluginMigrationSecret{}, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{}}, projectRequired, nil, "", sources)

	// A configured MCP switch alone is not enough to claim an external
	// declaration migrated. Only an enabled declaration with its matching ready
	// adapter expresses intent. Connection data and legacy task evidence remain
	// host-owned regardless of the supported lifecycle.
	externalIntent := legacyExternalIntentFor(mcp, legacyExternal)
	externalEnabled := externalIntent.synchronous && packages[externalHTTPPluginID].externalSync || externalIntent.jobs && packages[externalHTTPPluginID].externalJob || externalIntent.polls && packages[externalHTTPPluginID].externalPoll || externalIntent.mcp && packages[externalHTTPPluginID].externalMCP
	externalRequired := []string{}
	externalUnmapped := []string{}
	if externalIntent.fromPluginLedger {
		externalUnmapped = append(externalUnmapped, "plugins.json:host_retained_declarations_and_credentials")
	}
	if externalIntent.fromMCPConfig {
		externalUnmapped = append(externalUnmapped, "mcp-config.json:host_retained_connections_and_credentials")
	}
	if externalIntent.jobs {
		// Existing tasks are never reconstructed from a migration preview. The
		// job adapter will retain the original ID and mark missing immutable
		// origin/configuration proof as needs_review instead of replaying it.
		externalUnmapped = append(externalUnmapped, "legacy_external_tasks:origin_and_configuration_proof_required")
	}
	if externalIntent.polls {
		externalUnmapped = append(externalUnmapped, "legacy_external_tasks:origin_and_configuration_proof_required")
	}
	if externalIntent.declared && !externalEnabled {
		externalRequired = append(externalRequired, "no_matching_supported_external_tools")
	}
	if externalIntent.synchronous && !packages[externalHTTPPluginID].externalSync {
		externalRequired = append(externalRequired, "official_sync_external_adapter_not_declared_by_external_tools_plugin")
	}
	if externalIntent.jobs && !packages[externalHTTPPluginID].externalJob {
		externalRequired = append(externalRequired, "official_external_job_adapter_not_declared_by_external_tools_plugin")
	}
	if externalIntent.polls && !packages[externalHTTPPluginID].externalPoll {
		externalRequired = append(externalRequired, "official_external_poll_adapter_not_declared_by_external_tools_plugin")
	}
	if externalIntent.mcp && !packages[externalHTTPPluginID].externalMCP {
		externalRequired = append(externalRequired, "official_external_mcp_adapter_not_declared_by_external_tools_plugin")
	}
	if externalIntent.rawStdio {
		externalUnmapped = append(externalUnmapped, "mcp_stdio_runtime_package_required")
	}
	if externalIntent.unsupported {
		externalUnmapped = append(externalUnmapped, "external_declarations:unsupported_runtime_retained")
	}
	external := migrationStep(externalHTTPPluginID, packages, externalEnabled, map[string]json.RawMessage{}, map[string]pluginMigrationSecret{}, pluginhost.ConfigUpdate{Values: map[string]json.RawMessage{}}, externalRequired, externalUnmapped, "", sources)
	return pluginMigrationAccount{Account: m.native.Account(), SourceHashes: sources, SourceErrors: sourceErrors, Plugins: []pluginMigrationStep{media, speechStep, musicStep, web, share, project, external}}, nil
}

func (m *Manager) pluginMigrationPreview(ctx context.Context) (pluginMigrationPreview, error) {
	packages := migrationCatalog()
	managers := []*Manager{m}
	if registry := m.accounts; registry != nil {
		registry.mu.Lock()
		managers = managers[:0]
		for _, item := range registry.items {
			if item != nil && !item.record.Removed && item.manager != nil {
				managers = append(managers, item.manager)
			}
		}
		registry.mu.Unlock()
	}
	sort.Slice(managers, func(i, j int) bool { return managers[i].native.Account() < managers[j].native.Account() })
	out := pluginMigrationPreview{SchemaVersion: pluginMigrationPreviewVersion, Kind: "plugin_migration_preview", ReadOnly: true}
	for _, manager := range managers {
		item, err := manager.pluginMigrationPreviewFor(ctx, packages)
		if err != nil {
			return pluginMigrationPreview{}, err
		}
		out.Accounts = append(out.Accounts, item)
	}
	return out, nil
}
