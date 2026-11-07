package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/agentworkspace"
	"wechat-bot/internal/billing"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/devicespec"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/sending"
)

type Native interface {
	bot.Source
	Ready() bool
	SendText(context.Context, sendingRequest) (sendingReceipt, error)
	Status(context.Context) (linuxnative.Status, error)
	Groups(context.Context, string) ([]linuxnative.Group, error)
	BeginLogin(context.Context, string) (linuxnative.LoginOperation, error)
	LoginStatus(context.Context, string) (linuxnative.LoginOperation, error)
	LoginQR(context.Context, string, int) ([]byte, error)
}

// Keep the public native client interface aligned with the shared sender.
// Aliases preserve its exact method signature for fake/native implementations.
type sendingRequest = sending.TextRequest
type sendingReceipt = sending.Receipt

type LogEntry struct {
	MessageTime            string                   `json:"message_time,omitempty"`
	SenderName             string                   `json:"sender_name,omitempty"`
	RelatedMessageID       string                   `json:"related_message_id,omitempty"`
	DecisionNote           string                   `json:"decision_note,omitempty"`
	RetryAttempts          int                      `json:"retry_attempts,omitempty"`
	RetryAt                string                   `json:"retry_at,omitempty"`
	ToolCacheHit           bool                     `json:"tool_cache_hit,omitempty"`
	ToolRequests           int                      `json:"tool_requests,omitempty"`
	ToolRetries            int                      `json:"tool_retries,omitempty"`
	ToolHTTPStatus         int                      `json:"tool_http_status,omitempty"`
	ToolCode               string                   `json:"tool_code,omitempty"`
	ToolStage              string                   `json:"tool_stage,omitempty"`
	ModelHTTPStatus        int                      `json:"model_http_status,omitempty"`
	ModelAttempts          int                      `json:"model_attempts,omitempty"`
	ModelMaxAttempts       int                      `json:"model_max_attempts,omitempty"`
	ModelStage             string                   `json:"model_stage,omitempty"`
	ModelRetryDelayMS      int64                    `json:"model_retry_delay_ms,omitempty"`
	ModelFinishReason      string                   `json:"model_finish_reason,omitempty"`
	ModelContentBytes      int                      `json:"model_content_bytes,omitempty"`
	ModelJSONValid         bool                     `json:"model_json_valid,omitempty"`
	ModelFormatAttempt     int                      `json:"model_format_attempt,omitempty"`
	ModelMaxFormatAttempts int                      `json:"model_max_format_attempts,omitempty"`
	ModelProtocolCode      string                   `json:"model_protocol_code,omitempty"`
	ReviewFailure          *companion.ReviewFailure `json:"review_failure,omitempty"`
	CallID                 string                   `json:"call_id,omitempty"`
	Tool                   string                   `json:"tool,omitempty"`
	Effect                 string                   `json:"effect,omitempty"`
	TargetURL              string                   `json:"target_url,omitempty"`
	Query                  string                   `json:"query,omitempty"`
	DurationMS             int64                    `json:"duration_ms,omitempty"`
	Bytes                  int64                    `json:"bytes,omitempty"`
	MediaCount             int                      `json:"media_count,omitempty"`
	SendID                 string                   `json:"send_id,omitempty"`
	SchemaVersion          int                      `json:"schema_version,omitempty"`
	ID                     string                   `json:"id,omitempty"`
	Time                   string                   `json:"time"`
	Event                  string                   `json:"event"`
	Category               string                   `json:"category,omitempty"`
	ThreadID               string                   `json:"thread_id,omitempty"`
	SourceKind             string                   `json:"source_kind,omitempty"`
	Source                 string                   `json:"source,omitempty"`
	Sender                 string                   `json:"sender,omitempty"`
	Content                string                   `json:"content,omitempty"`
	Reason                 string                   `json:"reason,omitempty"`
	Status                 string                   `json:"status,omitempty"`
	MessageID              string                   `json:"message_id,omitempty"`
	Origin                 string                   `json:"origin,omitempty"`
}

type BotStatus struct {
	State     string `json:"state"`
	LastEvent string `json:"last_event"`
}
type Manager struct {
	enrollment          enrollmentRuntime
	packageMu           sync.Mutex
	packages            *pluginhost.Store
	packagesClosed      bool
	packageRuntime      *pluginruntime.Controller
	packageRuntimeReady bool
	managedServices     *pluginruntime.ManagedServices
	managedServicesOnce sync.Once
	musicIdleMu         sync.Mutex
	musicIdleTimer      *time.Timer
	musicIdleGeneration uint64
	memoryRunnerMu      sync.Mutex
	memoryRunnerWG      sync.WaitGroup
	memoryRunners       map[string]context.CancelFunc
	memoryRunnersClosed bool
	memoryServiceMu     sync.Mutex
	workspaceDir        string
	directoryMu         sync.Mutex
	accounts            *accountRegistry
	fish                *fishidentity.Registry
	canvasMu            sync.Mutex
	friendsMu           sync.Mutex
	friends             map[string]friendEntry
	workspace           *agentworkspace.Store
	processes           *agenthost.Jobs
	mobile              deviceService
	mobileRetired       bool
	deviceIO            devicespec.IOHost
	agent               *agentControl
	surfHistory         *surfHistoryStore
	surfInteractions    *surfInteractionStore
	billing             *billing.Store
	balance             balanceCache
	assetMu             sync.Mutex
	crossSendMu         sync.Mutex
	storageError        string
	drainTicket         string
	drainExpires        time.Time
	drainTimer          *time.Timer
	mediaSeeds          []bot.MediaSeed
	surf                surfState
	dir                 string
	stateDir            string
	native              Native
	op                  sync.Mutex
	mu                  sync.Mutex
	config              Config
	status              BotStatus
	logs                []LogEntry
	journal             []LogEntry
	logSequence         uint64
	cancel              context.CancelFunc
	done                chan struct{}
	memory              *companion.Store
	tools               *toolStore
	actions             *wechatapp.ActionLedger
	mcp                 mcpSettings
	plugins             *pluginLedger
}

// NewManager constructs a root manager without a native device transport.
func NewManager(dir, stateDir string, native Native) (*Manager, error) {
	return newManager(dir, stateDir, native, nil, false, nil)
}

// NewManagerWithDeviceIO attaches the trusted transport for a legacy host.
func NewManagerWithDeviceIO(dir, stateDir string, native Native, deviceIO devicespec.IOHost) (*Manager, error) {
	return newManager(dir, stateDir, native, nil, false, deviceIO)
}

// NewManagerWithRetiredDeviceControl serves existing Windows deployments with
// access to historical device records but no device control runtime.
func NewManagerWithRetiredDeviceControl(dir, stateDir string, native Native) (*Manager, error) {
	return newManager(dir, stateDir, native, nil, true, nil)
}

func newManager(dir, stateDir string, native Native, fish *fishidentity.Registry, mobileRetired bool, deviceIO devicespec.IOHost) (*Manager, error) {
	if native == nil || dir == "" {
		return nil, errors.New("missing admin runtime configuration")
	}
	var err error
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if stateDir != "" {
		stateDir, err = filepath.Abs(stateDir)
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("管理目录不可用")
	}
	if fish == nil {
		fish = fishidentity.New(dir)
	}
	c, err := loadConfig(dir)
	if err != nil {
		return nil, err
	}
	memory, err := companion.NewStore(filepath.Join(dir, "companion"))
	if err != nil {
		return nil, err
	}
	memoryInitialized := false
	defer func() {
		if !memoryInitialized {
			memory.Close()
		}
	}()
	tools, err := newToolStore(dir)
	if err != nil {
		return nil, err
	}
	actions, err := wechatapp.OpenActionLedger(dir)
	if err != nil {
		return nil, err
	}
	externalConfiguration, err := newExternalConfigurationRepository(dir, native.Account())
	if err != nil {
		return nil, err
	}
	mcp := externalConfiguration.snapshot().Settings
	plug := newPluginLedgerForRepository(externalConfiguration)
	if stateDir == "" {
		stateDir = filepath.Join(dir, "bot-state")
	}
	ledger, err := billing.Open(dir)
	if err != nil {
		return nil, err
	}
	surf, surfErr := loadSurf(dir)
	if surfErr != nil {
		return nil, surfErr
	}
	agent, err := newAgentControl(dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{agent: agent, dir: dir, stateDir: stateDir, native: native, config: c, status: BotStatus{State: "disabled"}, logs: []LogEntry{}, memory: memory, tools: tools, actions: actions, mcp: mcp, plugins: plug, fish: fish, mobileRetired: mobileRetired, deviceIO: deviceIO}
	externalConfiguration.openStore, externalConfiguration.owned = m.packageStore, false
	m.workspace, err = agentworkspace.Open(filepath.Join(dir, "agent-workspace"))
	if err != nil {
		return nil, err
	}
	workspaceInitialized := false
	defer func() {
		if !workspaceInitialized {
			m.workspace.Close()
		}
	}()
	m.processes, err = agenthost.OpenJobs(filepath.Join(dir, "agent-processes"))
	if err != nil {
		return nil, err
	}
	if err = m.configureWorkspaceDirectory(); err != nil {
		m.processes.Close()
		return nil, err
	}
	if err = m.configureManagedNotes(); err != nil {
		m.processes.Close()
		return nil, err
	}
	if err = m.initializeWorkspace(); err != nil {
		m.processes.Close()
		return nil, err
	}
	m.mobile = m.newMobileService()
	m.surf = surf
	// Only an explicitly present and valid catalogue can authoritatively remove
	// derived media search records. A copied SQLite database may legitimately
	// arrive before its media-assets manifest during recovery.
	if assets, err := m.loadAssetsSnapshot(); err == nil && assets != nil {
		if err = m.indexMemoryAssets(assets); err != nil {
			return nil, err
		}
		sources := map[string]map[string]companion.MediaMemory{}
		for _, a := range assets {
			if a.Account == native.Account() {
				if sources[a.Scope] == nil {
					sources[a.Scope] = map[string]companion.MediaMemory{}
				}
				source := companion.MediaMemory{URL: a.URL, CachedMIME: a.MIME, ParentAssetID: a.ParentAssetID}
				if a.FirstRequest != nil {
					source.RequestTraceID, source.RequesterID = a.FirstRequest.TraceID, a.FirstRequest.RequesterID
				}
				sources[a.Scope][a.ID] = source
			}
		}
		// Startup owns this account's operation ledger until NewManager returns.
		joinLegacySpeechScripts(native.Account(), agent.data.Operations, sources)
		if err = memory.BackfillMediaMetadata(sources); err != nil {
			return nil, err
		}
	}
	m.surfHistory, err = newSurfHistory(dir)
	if err != nil {
		return nil, err
	}
	m.surfInteractions, err = newSurfInteractionStore(dir)
	if err != nil {
		return nil, err
	}
	m.billing = ledger
	if data, readErr := os.ReadFile(filepath.Join(dir, "audit.json")); readErr == nil {
		if len(data) > 128<<10 || json.Unmarshal(data, &m.logs) != nil || len(m.logs) > 100 {
			return nil, errors.New("运行审计记录无效，未自动覆盖")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, errors.New("运行审计记录不可读")
	}
	for i := range m.logs {
		if m.logs[i].ID == "" {
			m.logs[i].ID = m.nextLogIDLocked()
		}
	}
	if err := m.loadJournal(); err != nil {
		return nil, err
	}
	// Recovery diagnostics must append to the loaded journal, never overwrite
	// existing history while manager construction is still in progress.
	if err = m.restoreCrossMedia(); err != nil {
		m.processes.Close()
		m.mobile.Close()
		return nil, err
	}
	if err := m.surfHistory.importAccepted(m.journal); err != nil {
		return nil, err
	}
	receiptCtx, cancelReceipts := context.WithTimeout(context.Background(), 3*time.Second)
	m.importOwnerSurfReceipts(receiptCtx)
	cancelReceipts()
	if archive, ok := m.mobile.(*mobileArchiveService); ok {
		if err := archive.retire(); err != nil {
			m.log("device_archive_retirement_failed")
		}
	}
	if c.ConnectionPaused {
		m.setState("paused")
	} else if c.Enabled {
		m.start(c)
	}
	workspaceInitialized = true
	memoryInitialized = true
	if pending, e := m.memory.PendingRebuilds(context.Background()); e == nil {
		for _, id := range pending {
			if _, e = m.memoryScope(id); e == nil {
				_ = m.startMemoryRebuild(id)
			}
		}
	}
	return m, nil
}
func (m *Manager) Config() ConfigView {
	m.mu.Lock()
	c := cloneConfig(m.config)
	m.mu.Unlock()
	if c.PrivateEnabled && c.PrivateAllFriends {
		c.friendIDs = m.cachedFriends()
	}
	return c.view()
}
func cloneConfig(c Config) Config {
	if c.Companion.Grounding != nil {
		copy := *c.Companion.Grounding
		c.Companion.Grounding = &copy
	}
	if c.Companion.Context != nil {
		copy := *c.Companion.Context
		c.Companion.Context = &copy
	}
	if c.Companion.Conversation != nil {
		copy := *c.Companion.Conversation
		if copy.GroupAggregateMS != nil {
			ms := *copy.GroupAggregateMS
			copy.GroupAggregateMS = &ms
		}
		c.Companion.Conversation = &copy
	}
	c.DirectIDs = append([]string{}, c.DirectIDs...)
	c.friendIDs = append([]string{}, c.friendIDs...)
	c.GroupRules = append([]GroupRule{}, c.GroupRules...)
	c.Companion.ProactiveThreads = append([]string{}, c.Companion.ProactiveThreads...)
	c.Companion.ProactiveRules = append([]companion.ProactiveRule{}, c.Companion.ProactiveRules...)
	return c
}
func (m *Manager) log(event string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.LastEvent = event
	m.logs = append(m.logs, LogEntry{ID: m.nextLogIDLocked(), Time: time.Now().UTC().Format(time.RFC3339Nano), Event: event, Category: eventCategory(event)})
	if len(m.logs) > 100 {
		m.logs = m.logs[len(m.logs)-100:]
	}
	data, err := json.Marshal(m.logs)
	if err != nil || writePrivate(filepath.Join(m.dir, "audit.json"), data) != nil {
		m.status.LastEvent = "audit_persistence_failed"
	}
}

func (m *Manager) BotStatus() BotStatus { m.mu.Lock(); defer m.mu.Unlock(); return m.status }

func (m *Manager) Save(input ConfigInput) (ConfigView, error) {
	m.op.Lock()
	defer m.op.Unlock()
	return m.saveLocked(input)
}
func (m *Manager) saveLocked(input ConfigInput) (ConfigView, error) {
	m.mu.Lock()
	previous := cloneConfig(m.config)
	m.mu.Unlock()
	next := cloneConfig(input.Config)
	next.ConnectionPaused = previous.ConnectionPaused
	if next.Companion.Name == "" {
		next.Companion = previous.Companion
	}
	next.Companion = companion.Normalize(next.Companion)
	if !input.ClearKey && next.APIKey == "" && previous.APIKey != "" {
		oldURL, oldErr := url.Parse(previous.BaseURL)
		newURL, newErr := url.Parse(next.BaseURL)
		if oldErr == nil && newErr == nil && (oldURL.Scheme != newURL.Scheme || !strings.EqualFold(oldURL.Host, newURL.Host)) {
			return ConfigView{}, errors.New("更换服务商地址时，请重新填入对应的 API Key，防止旧密钥发往其他服务商")
		}
	}
	if input.ClearKey {
		next.APIKey = ""
	} else if next.APIKey == "" {
		next.APIKey = previous.APIKey
	}
	if err := validate(next); err != nil {
		return ConfigView{}, err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return ConfigView{}, errors.New("配置编码失败")
	}
	// Stop/cancel old work before changing its role/rules. Interrupted sends
	// remain claimed in the same checkpoint and will not be replayed.
	if err = m.stop(); err != nil {
		return ConfigView{}, err
	}
	if err = writePrivate(filepath.Join(m.dir, "config.json"), data); err != nil {
		if previous.Enabled {
			m.start(previous)
		}
		return ConfigView{}, err
	}
	if next.APIKey == "" {
		_ = os.Remove(filepath.Join(m.dir, "provider-key"))
	}
	m.mu.Lock()
	m.config = next
	m.mu.Unlock()
	m.log("configuration_saved")
	if next.Enabled {
		m.start(next)
	}
	return next.view(), nil
}
func (m *Manager) Toggle(enabled bool) error {
	m.op.Lock()
	defer m.op.Unlock()
	if enabled && m.connectionPaused() {
		return errors.New("微信连接已暂停，请先从应用连接恢复")
	}
	if !enabled && m.mobile != nil {
		if err := m.mobile.StopAll(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	c := cloneConfig(m.config)
	m.mu.Unlock()
	c.Enabled = enabled
	_, err := m.saveLocked(ConfigInput{Config: c})
	return err
}
func (m *Manager) stop() error {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.drainTicket = ""
	if m.drainTimer != nil {
		m.drainTimer.Stop()
	}
	if cancel != nil {
		m.status.State = "stopping"
		cancel()
	}
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			return errors.New("机器人仍在停止，请稍后再试")
		}
	}
	m.mu.Lock()
	m.cancel = nil
	m.done = nil
	m.status.State = "disabled"
	if m.config.ConnectionPaused {
		m.status.State = "paused"
	}
	m.mu.Unlock()
	return nil
}
func (m *Manager) Close() error {
	m.enrollment.close()
	// Stop task owners while their plugin sessions and private job store are
	// still available to checkpoint cancellation and reap native child trees.
	var mobileErr error
	if m.mobile != nil {
		mobileErr = m.mobile.Close()
	}
	m.packageMu.Lock()
	m.packagesClosed = true
	packages, packageRuntime := m.packages, m.packageRuntime
	m.packageMu.Unlock()
	var cleanupErr error
	if !m.mobileRetired && m.deviceIO != nil {
		cleanupErr = m.deviceIO.Cleanup()
	}
	var packageErr error
	if packageRuntime != nil {
		packageErr = packageRuntime.Close()
	}
	if packages != nil && packageErr == nil {
		packageErr = packages.Close()
	}
	m.closeWindowsMusic()
	m.closeMemoryRunners()
	if m.accounts != nil {
		m.accounts.close()
	}
	m.op.Lock()
	defer m.op.Unlock()
	if m.processes != nil {
		m.processes.Close()
	}
	err := errors.Join(mobileErr, cleanupErr, packageErr, m.stop())
	if m.workspace != nil {
		err = errors.Join(err, m.workspace.Close())
	}
	if m.memory != nil {
		err = errors.Join(err, m.memory.Close())
	}
	return err
}
func (m *Manager) setState(state string) { m.mu.Lock(); m.status.State = state; m.mu.Unlock() }
func (m *Manager) makeModel(c Config) (*deepseek.Client, error) {
	if c.APIKey == "" {
		return nil, errors.New("请先保存 API Key")
	}
	keyFile := filepath.Join(m.dir, "provider-key")
	if err := writePrivate(keyFile, []byte(c.APIKey)); err != nil {
		return nil, err
	}
	toolTokens := c.MaxTokens
	if !c.ThinkingEnabled && c.Companion.Enabled && m.agent != nil && m.agent.config().Enabled {
		toolTokens = 4096
	}
	return deepseek.New(deepseek.Config{ThinkingEnabled: &c.ThinkingEnabled, ReasoningEffort: c.ReasoningEffort, BaseURL: c.BaseURL, KeyFile: keyFile, Model: c.Model, SystemPrompt: c.SystemPrompt, MaxTokens: c.MaxTokens, ToolMaxTokens: toolTokens, Compatible: c.Provider == "openai-compatible", OnUsage: func(u deepseek.Usage) {
		if m.billing != nil {
			if err := m.billing.Record(modelForBilling(c), u); err != nil {
				m.log("usage_record_save_failed")
			}
		}
	}})
}
func (m *Manager) start(c Config) {
	if c.ConnectionPaused {
		m.setState("paused")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel = cancel
	m.drainTicket = ""
	m.done = done
	m.status.State = "waiting_login"
	m.mu.Unlock()
	go func() {
		defer close(done)
		for !m.native.Ready() {
			if m.draining() {
				m.setState("drained")
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if m.draining() {
			m.setState("drained")
			return
		}
		if ctx.Err() != nil {
			return
		}
		provider, err := m.makeModel(c)
		if err != nil {
			m.setState("error")
			m.log("provider_configuration_error")
			return
		}
		var model bot.Model = provider
		if c.Companion.Enabled {
			character, err := companion.New(c.sharedCompanion(), m.memory, provider, c.SystemPrompt)
			if err != nil {
				m.setState("error")
				m.log("character_configuration_error")
				return
			}
			character.Images = m.tools.imageAssets(m.native.Account())
			character.MemoryEmbedding = func() companion.EmbeddingConfig {
				cfg, e := m.loadMemoryServices()
				if e != nil {
					return companion.EmbeddingConfig{}
				}
				return cfg.Embedding
			}
			character.MemoryOptions = func() companion.MemorySettings {
				cfg, e := m.loadMemoryServices()
				if e != nil {
					return companion.MemorySettings{}
				}
				return cfg.Settings
			}
			character.ReplyAuditOptions = func() companion.ReplyAuditSettings {
				cfg, e := m.loadMemoryServices()
				if e != nil {
					return companion.ReplyAuditSettings{Enabled: false, RetentionDays: 7}
				}
				return cfg.Settings.AuditConfig()
			}
			if names, ok := m.native.(companion.NameLookup); ok {
				character.Names = names
			}
			access, maxCalls, toolErr := m.toolAccess()
			if toolErr != nil {
				m.log("mcp_configuration_error")
			} else if access != nil {
				defer access.hub.Close()
				character.Tools = access
				character.MaxToolCalls = maxCalls
				character.ToolLog = func(event string) {
					// permittedTools already retains the scoped result of every call.
					// Do not add a second anonymous system row for the same action.
					if event != "mcp_tool_call_succeeded" && event != "mcp_tool_call_failed" {
						m.log(event)
					}
				}
			}
			model = character
		}
		rules := map[string]bot.GroupRule{}
		for _, r := range c.GroupRules {
			rules[r.ID] = bot.GroupRule{Mode: r.Mode, Prefix: r.Prefix, Persona: r.Persona, ForceReplyWhenMentioned: r.ForceReplyWhenMentioned}
		}
		friendCtx, cancelFriends := context.WithTimeout(ctx, 8*time.Second)
		m.warmFriends(friendCtx)
		cancelFriends()
		c.friendIDs = m.cachedFriends()
		worker, err := bot.New(bot.Config{BeforeSubmit: m.checkWechatSubmission, ObservationFailed: m.queueOwnerDetail, MediaRepairRequired: m.queueMediaRepair, BacklogSettled: m.settleBacklogReviews, AuthorizeThread: m.authorizeConversation, AgentTaskActive: m.agentTaskActive, AgentTask: m.claimAgentTask, AgentTaskDone: m.finishAgentTask, AgentTaskRetry: m.retryAgentTask, AgentTaskDelivery: m.updateAgentDelivery, Draining: m.draining, MediaSeeds: m.takeMediaSeeds, MediaSeedKnown: m.crossSeedKnown, MediaSeedAllowed: m.crossSeedAllowed, MediaSeedSettled: m.settleCrossMediaPart, MediaSeedReceipt: m.crossPartReceipt, StateDir: m.stateDir, SurfTask: m.claimSurf, SurfAllowed: m.surfAllowed, Options: &c.Behavior, Analyzer: mediaUnderstanding{manager: m}, AllowedThreads: c.allowed(), Prefix: c.Prefix, MaxAge: time.Duration(c.MaxAgeSeconds) * time.Second, GroupRules: rules, Log: m.log, Record: m.record}, m.native, model, m.native)
		if err != nil {
			m.setState("error")
			m.log("bot_configuration_error")
			return
		}
		reminderCtx, cancelReminders := context.WithCancel(ctx)
		if character, ok := model.(*companion.Engine); ok {
			memoryCtx, cancelMemory := context.WithCancel(ctx)
			memoryDone := make(chan struct{})
			go func() { defer close(memoryDone); m.runMemoryMaintenance(memoryCtx, character) }()
			defer func() { cancelMemory(); <-memoryDone }()
		}
		reminderDone := make(chan struct{})
		go func() { defer close(reminderDone); m.runReminders(reminderCtx) }()
		defer func() { cancelReminders(); <-reminderDone }()
		m.setState("running")
		m.log("bot_starting")
		err = worker.Run(ctx)
		if err == nil && ctx.Err() == nil && m.draining() {
			m.setState("drained")
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.setState("error")
			m.log("bot_stopped_check_connection_or_checkpoint")
			m.log(botFailureReason(err))
			m.record(bot.Record{Event: "bot_runtime_failed", Category: "error", Origin: "system", Status: "failed", Reason: botFailureReason(err)})
		}
	}()
}
func (m *Manager) TestModel(ctx context.Context) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	c := cloneConfig(m.config)
	m.mu.Unlock()
	model, err := m.makeModel(c)
	if err != nil {
		return err
	}
	_, err = model.Complete(ctx, "请只回复：连接成功。")
	if err != nil {
		m.log("model_test_failed")
		d, _ := deepseek.ErrorDiagnostics(err)
		m.record(bot.Record{Event: "provider_test_failed", Category: "error", Origin: "manual_model", Reason: d.Code, Status: "failed", ModelHTTPStatus: d.HTTPStatus, ModelAttempts: d.Attempts, ModelMaxAttempts: d.MaxAttempts, ModelStage: d.Stage, DurationMS: d.DurationMS})
		return errors.New(deepseek.UserMessage(err))
	}
	m.log("model_test_succeeded")
	return nil
}
