(() => {
  "use strict";
  const $ = (selector) => document.querySelector(selector);
  const state = { csrf: "", home: null, model: null, busy: false, chat: newChatView() };
  const tabs = ["chat", "apps", "project-tools", "settings"];
  let activeView = "chat";
  let apps = newAppView();
  let projectTools = newProjectToolsView();
  const loginView = $("#login-view"), homeView = $("#home-view"), localWorkspace = $("#local-workspace"), message = $("#page-message");
  const loginSubmit = $("#login-submit"), initializeButton = $("#initialize-button"), accessButton = $("#access-button"), logoutButton = $("#logout-button");
  const settingsAccessButton = $("#settings-access-button"), settingsLogoutButton = $("#settings-logout-button"), chatStream = $("#chat-stream");

  function showMessage(text, kind = "") { message.textContent = text || ""; message.className = `page-message${kind ? ` is-${kind}` : ""}`; }
  function safeServerMessage(body, fallback) { const value = body && (body.error || body.message || body.detail); return typeof value === "string" && value.trim() ? value.trim().slice(0, 280) : fallback; }
  function responseMessage(status) {
    if (status === 401 || status === 403) return "登录状态已失效，请重新验证。";
    if (status === 409) return "当前请求正在处理，或已被新的状态替代。";
    if (status === 503) return "还没有可用的聊天模型，请先完成模型设置。";
    if (status === 502) return "模型暂时没有返回结果。可以稍后手动重试这条消息。";
    return "暂时无法完成操作，请确认本机服务后再试。";
  }
  async function request(url, options = {}) {
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");
    if (options.body) headers.set("Content-Type", "application/json");
    if (state.csrf && options.method && options.method !== "GET") headers.set("X-CSRF-Token", state.csrf);
    let response;
    try { response = await fetch(url, { ...options, headers, credentials: "same-origin" }); }
    catch (_) { const error = new Error("无法连接本机服务。"); error.status = 0; throw error; }
    let body = null;
    try { body = await response.json(); } catch (_) { /* Do not render non-JSON error bodies. */ }
    if (!response.ok) { const error = new Error(safeServerMessage(body, responseMessage(response.status))); error.status = response.status; error.body = body; throw error; }
    return body || {};
  }
  function authenticated(session) { return Boolean(session && (session.authenticated || session.logged_in || session.csrf)); }
  function setBusy(busy) {
    state.busy = busy;
    for (const node of [loginSubmit, initializeButton, accessButton, logoutButton, settingsAccessButton, settingsLogoutButton]) node.disabled = busy;
  }
  function chatConfigured() { return Boolean(state.model && state.model.key_configured); }
  function setActiveView(view) {
    if (activeView === "project-tools" && view !== "project-tools") { stopOBSPreviewOnLeave(); invalidateOBSDeploymentView(); }
    if (activeView === "chat" && view !== "chat") { window.clearTimeout(state.chat.poll); state.chat.poll = 0; }
    activeView = view;
    for (const name of tabs) {
      const tab = $(`#${name}-tab`), selected = name === view;
      $(`#${name}-view`).hidden = !selected;
      tab.classList.toggle("is-active", selected);
      tab.setAttribute("aria-selected", String(selected));
      tab.tabIndex = selected ? 0 : -1;
    }
    if (view === "apps") { loadApps(); scheduleAppLogin(apps); }
    else { window.clearTimeout(apps.loginTimer); stopDouyinWebLoginPolling(apps); stopDouyinInteractionWork(apps); stopDouyinLiveInspection(apps); }
    if (view === "project-tools") loadProjectTools();
    if (view === "chat" && state.home?.initialized && !document.hidden) void loadChat(true);
  }
  function newChatView() {
    return { items: [], nextCursor: "", hasMore: false, busy: false, backgroundPending: false, loading: false, refreshRequested: false, pending: new Map(), failed: new Map(), poll: 0 };
  }
  function newAppView() {
    return { loading: false, installing: "", catalog: null, registered: null, personas: new Map(), connections: new Map(), accountStatuses: new Map(), setupBusy: false, provisionKey: "", logins: new Map(), selectedLogin: "", loginTimer: 0, workerDrafts: new Map(), historyDrafts: new Map(), attachmentDrafts: new Map(), autoImagesDrafts: new Map(), autoVideosDrafts: new Map(), mediaReplyDrafts: new Map(), groupMuteDrafts: new Map(), groupRuleDrafts: new Map(), workDrafts: new Map(), resumeDrafts: new Map(), editWorker: "",
      douyinWebLogin: { value: null, loaded: false, loading: false, applying: "", error: "", poll: 0, generation: 0 },
      douyinLiveClient: { path: "", result: null, busy: false, error: "", generation: 0, controller: null },
      bilibiliPublic: null, bilibiliDraft: null, bilibiliSaving: false, bilibiliError: "", bilibiliAuthorization: null, bilibiliAuthorizationError: "", bilibiliAuthorizationSaving: false,
      bilibiliOAuth: { saving: false, authorizeURL: "", expiresAt: "" },
      bilibiliWork: { page: null, cursors: [0], reading: false, readID: 0, pending: new Map(), reconciliation: new Map(), error: "", notice: "", actionError: false },
      douyinWork: { page: null, cursors: [0], reading: false, readID: 0, pending: new Map(), error: "", notice: "", actionError: false },
      douyinActivityTab: "publications",
      douyinInteractions: { page: null, cursors: [0], reading: false, readID: 0, epoch: 0, pending: new Map(), error: "", notice: "", actionError: false } };
  }
  function newProjectToolsView() {
    return { loading: false, applying: "", tools: [], vision: { loading: false, saving: false, generation: 0, server: null, draft: null, apiKey: "", clearKey: false, approved: false, error: "", notice: "" }, comfyDeployment: newDeploymentState(), yueDeployment: newDeploymentState(), obsDeployment: { generation: 0, loading: false, saving: false, readable: false, server: null, archivePath: "", approved: false, needsRefresh: false, error: "", notice: "" }, obsPreview: { loading: false, applying: false, polling: false, starting: false, stopping: false, timer: 0, generation: 0, configured: false, port: 4455, configRevision: 0, active: false, enabled: false, scene: "", observedAt: "", imageURL: "", error: "" } };
  }
  function appElement(tag, text, className = "") {
    const node = document.createElement(tag);
    node.textContent = text;
    if (className) node.className = className;
    return node;
  }
  function appFeedback(text, warn = false) {
    const feedback = $("#apps-feedback");
    feedback.textContent = text;
    feedback.classList.toggle("is-warn", warn);
  }
  function projectToolsFeedback(text, warn = false) {
    const feedback = $("#project-tools-feedback");
    feedback.textContent = text || "";
    feedback.classList.toggle("is-warn", Boolean(warn));
  }
  function appSessionExpired(results) {
    if (!results.some((result) => result.status === "rejected" && result.reason?.status === 401)) return false;
    state.csrf = "";
    showLogin();
    showMessage("登录状态已失效，请重新验证。", "warn");
    return true;
  }
  function appRows(result, key) {
    if (result.status !== "fulfilled" || !Array.isArray(result.value[key])) return null;
    return result.value[key].filter((item) => item && typeof item.id === "string" && item.id);
  }
  function appSourceRow(record, listing) {
    const row = appElement("div", "", "app-layer");
    const detail = appElement("div", "");
    let title = "接入版本状态未知", note = "刷新后可重新检查。";
    if (listing) {
      const bundledVersion = listing.version || "未知";
      const workspaceVersion = listing.installed_version || bundledVersion;
      if (listing.install_state === "installed") {
        title = "已安装";
        note = `随包版本 ${bundledVersion}；工作区版本 ${workspaceVersion}。接入版本用于本软件连接应用和更新接入功能，可在家目录的 Connected Apps 中维护。`;
      } else if (listing.install_state === "conflict") {
        title = "应用目录待整理";
        note = listing.installed_version ? `已有版本 ${listing.installed_version}，请先保留现有修改。` : "家目录已有同名内容，请先保留并整理该目录。";
      } else if (!listing.source_available) {
        title = "暂无可用版本";
        note = "当前发行包未提供此应用的接入版本，本地聊天仍可继续使用。";
      } else if (listing.install_state === "not_installed") {
        title = "可用版本";
        note = state.home?.home_access
          ? `随包版本 ${bundledVersion} 可用。接入版本用于本软件连接应用和更新接入功能；应用授权单独保存。`
          : "请在模型设置中恢复工作空间授权。";
      } else if (listing.install_state === "unavailable") {
        title = "安装状态暂不可读";
        note = "请检查工作空间是否可用。";
      }
    } else if (apps.catalog) {
      title = "暂无可用版本";
      note = "当前发行包没有此应用的接入版本。";
    }
    detail.append(appElement("strong", title), appElement("p", note));
    row.append(appElement("h5", "接入版本"), detail);
    if (title === "可用版本" && state.home?.home_access) {
      const busy = apps.installing === record.id;
      const button = appElement("button", busy ? "正在安装…" : "安装接入版本", "button compact primary");
      button.type = "button";
      button.disabled = Boolean(apps.installing) || apps.loading;
      button.addEventListener("click", () => installApp(listing));
      row.append(button);
    }
    return row;
  }
  function appSetupButton(text, action) {
    const button = appElement("button", text, "button compact secondary");
    button.type = "button";
    button.disabled = apps.setupBusy || apps.loading;
    button.addEventListener("click", action);
    return button;
  }
  const appLoginLabels = {
    starting: "正在生成二维码…", awaiting_scan: "请用微信扫描二维码",
    awaiting_approval: "请在微信上确认登录", succeeded: "微信登录已确认",
    expired: "二维码已过期", failed: "微信登录失败", cancelled: "微信登录已取消",
    rejected: "微信登录未获确认", needs_login: "需要重新扫码", challenge_required: "请在手机上核对登录验证",
    approval_required: "请在手机上核对登录验证", needs_review: "请在手机上核对登录验证", unknown: "微信登录状态暂不可读"
  };
  const pendingAppLogin = (op) => op && ["starting", "awaiting_scan", "awaiting_approval"].includes(op.state);
  const appLoginPath = (id) => "/api/apps/wechat/connections/" + encodeURIComponent(id) + "/login";
  const newAppSetupKey = () => crypto.randomUUID().replaceAll("-", "");
  const appSetupPost = (path, key) => request(path, { method: "POST", body: JSON.stringify({ request_id: key }) });

  function renderAppLogin(id, op) {
    const panel = appElement("div", "", "app-login");
    panel.append(appElement("p", appLoginLabels[op.state] || appLoginLabels.unknown));
    if (op.qr_available && op.state === "awaiting_scan") {
      const image = document.createElement("img");
      image.src = appLoginPath(id) + "/" + encodeURIComponent(op.id) + "/qr?generation=" + op.qr_generation;
      image.alt = "微信登录二维码";
      image.addEventListener("error", () => {
        image.remove();
        panel.append(appElement("p", "二维码暂不可读，可重新查看当前登录。"));
      }, { once: true });
      panel.append(image);
    }
    return panel;
  }
  function scheduleAppLogin(current, delay = 2200) {
    window.clearTimeout(current.loginTimer);
    const id = current.selectedLogin, entry = current.logins.get(id);
    if (current !== apps || activeView !== "apps" || !pendingAppLogin(entry?.op)) return;
    current.loginTimer = window.setTimeout(() => pollAppLogin(current, id, entry), delay);
  }
  async function pollAppLogin(current, id, entry) {
    const before = entry.op;
    try {
      const op = await request(appLoginPath(id) + "/" + encodeURIComponent(before.id));
      if (current !== apps || current.selectedLogin !== id || entry.op !== before || op.id !== before.id) return;
      entry.op = op;
      if (op.state !== before.state || op.qr_available !== before.qr_available || op.qr_generation !== before.qr_generation) renderApps();
      if (op.state === "succeeded") await loadApps();
    } catch (error) {
      if (current !== apps || current.selectedLogin !== id || appSessionExpired([{ status: "rejected", reason: error }])) return;
      if (error.status === 404) {
        entry.op = { ...before, state: "unknown", qr_available: false };
        renderApps();
      }
      appFeedback("暂时无法读取微信登录状态，可稍后重新查看。", true);
      scheduleAppLogin(current, 5000);
      return;
    }
    scheduleAppLogin(current);
  }
  async function runAppSetup(id = "") {
    const current = apps;
    if (current.setupBusy || current.loading) return;
    current.setupBusy = true;
    if (id) current.selectedLogin = id;
    appFeedback("");
    renderApps();
    try {
      if (!id) {
        current.provisionKey ||= newAppSetupKey();
        await appSetupPost("/api/apps/wechat/connections", current.provisionKey);
        current.provisionKey = "";
        if (current !== apps) return;
        appFeedback("账号已登记，可以扫码登录。");
        await loadApps();
      } else {
        let entry = current.logins.get(id);
        if (!entry || entry.op && !pendingAppLogin(entry.op)) {
          entry = { key: newAppSetupKey(), op: null };
          current.logins.set(id, entry);
        }
        if (entry.op) {
          await pollAppLogin(current, id, entry);
        } else {
          entry.op = await appSetupPost(appLoginPath(id), entry.key);
          scheduleAppLogin(current);
        }
      }
    } catch (error) {
      if (current === apps && !appSessionExpired([{ status: "rejected", reason: error }])) appFeedback(error.message, true);
    } finally {
      current.setupBusy = false;
      if (current === apps) renderApps();
    }
  }
  const appWorkerLabels = {
    disabled: "Agent 未启动", needs_setup: "Agent 待配置", waiting_login: "Agent 等待登录",
    running: "Agent 运行中", stopping: "Agent 正在停止", paused: "Agent 已暂停", error: "Agent 运行异常",
    cutover_blocked: "等待完成启用核对", rollback_frozen: "此连接已为回退停发"
  };
  function appWorkerControls(id, status) {
    const panel = appElement("div", "", "app-worker");
    const buttons = appElement("div", "", "app-worker-actions");
    const running = ["running", "waiting_login", "stopping", "cutover_blocked"].includes(status.worker);
    const edit = appSetupButton(apps.editWorker === id ? "收起回复范围" : "设置回复范围", () => {
      apps.editWorker = apps.editWorker === id ? "" : id;
      renderApps();
    });
    buttons.append(appElement("small", appWorkerLabels[status.worker]), edit);
    const toggle = appSetupButton(running ? "暂停 Agent" : "启动 Agent",
      () => runWorkerAction(id, "POST", "lifecycle", { action: running ? "stop" : "start" }));
    toggle.disabled ||= ["stopping", "rollback_frozen"].includes(status.worker) || !running && !status.configured_policy?.conversation_enabled;
    buttons.append(toggle);
    panel.append(buttons);
    panel.append(contactDirectoryControls(id), groupDirectoryControls(id), deliveryRecords(id), stationToolControls(id, false), stationToolControls(id, true));
    if (apps.editWorker !== id) return panel;
    const field = document.createElement("textarea");
    field.rows = 3;
    field.placeholder = "filehelper，或微信用户 / 群 ID";
    field.setAttribute("aria-label", "回复会话 ID");
    field.value = apps.workerDrafts.get(id) ?? (status.configured_policy?.reply_threads || []).join("\n");
    field.addEventListener("input", () => apps.workerDrafts.set(id, field.value));
    const history = document.createElement("textarea");
    history.rows = 2;
    history.placeholder = "可留空，或填写已允许回复的会话 ID";
    history.setAttribute("aria-label", "历史与成员读取会话 ID");
    history.value = apps.historyDrafts.get(id) ?? (status.configured_policy?.history_threads || []).join("\n");
    history.addEventListener("input", () => apps.historyDrafts.set(id, history.value));
    const attachments = document.createElement("textarea");
    attachments.rows = 2;
    attachments.placeholder = "可留空，或填写上方已允许回复的会话 ID";
    attachments.setAttribute("aria-label", "附件读取会话 ID");
    attachments.value = apps.attachmentDrafts.get(id) ?? (status.configured_policy?.attachment_threads || []).join("\n");
    attachments.addEventListener("input", () => apps.attachmentDrafts.set(id, attachments.value));
    const autoImages = document.createElement("input");
    autoImages.type = "checkbox";
    autoImages.checked = apps.autoImagesDrafts.get(id) ?? (status.configured_policy?.auto_observe_images === true);
    autoImages.setAttribute("aria-label", "自动读图");
    autoImages.addEventListener("change", () => apps.autoImagesDrafts.set(id, autoImages.checked));
    const autoImagesLabel = appElement("label", "", "switch-row");
    const autoImagesCopy = appElement("span");
    autoImagesCopy.append(appElement("strong", "自动读图"), appElement("small", "把获准会话中符合回复条件的新图片交给此连接配置的视觉服务。"));
    autoImagesLabel.append(autoImages, autoImagesCopy);
    const autoVideos = document.createElement("input");
    autoVideos.type = "checkbox";
    autoVideos.checked = apps.autoVideosDrafts.get(id) ?? (status.configured_policy?.auto_observe_videos === true);
    autoVideos.setAttribute("aria-label", "自动读视频");
    autoVideos.addEventListener("change", () => apps.autoVideosDrafts.set(id, autoVideos.checked));
    const autoVideosLabel = appElement("label", "", "switch-row");
    const autoVideosCopy = appElement("span");
    autoVideosCopy.append(appElement("strong", "自动读视频"), appElement("small", "把获准会话中符合回复条件的新视频交给此连接配置的音频与视觉工具。"));
    autoVideosLabel.append(autoVideos, autoVideosCopy);
    const mediaReplies = document.createElement("textarea");
    mediaReplies.rows = 2;
    mediaReplies.placeholder = "可留空，或填入已允许读取附件的会话 ID";
    mediaReplies.setAttribute("aria-label", "附件回复会话 ID");
    mediaReplies.value = apps.mediaReplyDrafts.get(id) ?? (status.configured_policy?.media_reply_threads || []).join("\n");
    mediaReplies.addEventListener("input", () => apps.mediaReplyDrafts.set(id, mediaReplies.value));
    const contactRead = document.createElement("input");
    contactRead.type = "checkbox";
    contactRead.checked = Boolean(status.configured_policy?.contact_read);
    contactRead.setAttribute("aria-label", "允许大肥鱼在本地聊天中查阅此通讯录");
    const contactReadLabel = appElement("label", "", "switch-row");
    const contactReadCopy = appElement("span");
    contactReadCopy.append(appElement("strong", "允许大肥鱼在本地聊天中查阅此通讯录"),
      appElement("small", "主人可在应用页直接查看。大肥鱼查询时，所读资料会提供给当前聊天模型。"));
    contactReadLabel.append(contactRead, contactReadCopy);
    const groupMutes = document.createElement("textarea");
    groupMutes.rows = 2;
    groupMutes.placeholder = "每行一个群 ID；留空会撤销授权";
    groupMutes.setAttribute("aria-label", "Agent 免打扰群 ID");
    groupMutes.value = apps.groupMuteDrafts.get(id) ?? (status.configured_policy?.group_mute_threads || []).join("\n");
    groupMutes.addEventListener("input", () => apps.groupMuteDrafts.set(id, groupMutes.value));
    const groupRules = appElement("div", "", "group-rule-settings");
    const normalizeReplySettings = (settings) => {
      if (!settings || typeof settings !== "object" || Array.isArray(settings)) return undefined;
      return {
        mode: ["all", "social"].includes(settings.mode) ? settings.mode : "mention",
        prefix: typeof settings.prefix === "string" ? settings.prefix : "",
        persona: typeof settings.persona === "string" ? settings.persona : "",
        force_reply_when_mentioned: settings.force_reply_when_mentioned === true
      };
    };
    const configuredRules = new Map((status.configured_policy?.group_rules || [])
      .filter((rule) => rule && typeof rule.thread_id === "string")
      .map((rule) => [rule.thread_id, {
        note: String(rule.note || ""),
        summarize_shares: Boolean(rule.summarize_shares),
        reply_settings: normalizeReplySettings(rule.reply_settings)
      }]));
    const readGroupRule = (threadID) => apps.groupRuleDrafts.get(id)?.get(threadID) || configuredRules.get(threadID) || { note: "", summarize_shares: false };
    const updateGroupRule = (threadID, patch) => {
      let drafts = apps.groupRuleDrafts.get(id);
      if (!drafts) {
        drafts = new Map();
        apps.groupRuleDrafts.set(id, drafts);
      }
      drafts.set(threadID, { ...readGroupRule(threadID), ...patch });
    };
    const replyGroups = () => field.value.split(/[\s，,]+/u).filter((threadID) => threadID.endsWith("@chatroom"));
    const renderGroupRules = () => {
      const groups = replyGroups();
      groupRules.replaceChildren(appElement("strong", "群聊偏好"));
      if (!groups.length) {
        groupRules.append(appElement("small", "在回复范围中加入群聊后，可为该群写备注或开启分享总结。"));
        return;
      }
      for (const threadID of groups) {
        const value = readGroupRule(threadID);
        const row = appElement("div", "", "group-rule-row");
        const note = document.createElement("input");
        note.value = value.note;
        note.placeholder = "本群偏好（可选）";
        note.setAttribute("aria-label", threadID + " 群聊备注");
        note.addEventListener("input", () => updateGroupRule(threadID, { note: note.value }));
        const summarize = document.createElement("input");
        summarize.type = "checkbox";
        summarize.checked = value.summarize_shares;
        summarize.disabled = !attachmentThreadIDs().includes(threadID);
        summarize.setAttribute("aria-label", threadID + " 自动总结视频号分享");
        summarize.addEventListener("change", () => updateGroupRule(threadID, { summarize_shares: summarize.checked }));
        const summarizeLabel = appElement("label", "", "switch-row");
        summarizeLabel.append(summarize, appElement("span", "收到视频号分享时自动总结"));
        row.append(appElement("small", threadID), note, summarizeLabel);
        if (summarize.disabled) row.append(appElement("small", "需先在“附件读取会话”中授权本群读取视频号；不会自动开启。"));
        const advanced = document.createElement("details");
        advanced.className = "group-rule-advanced";
        advanced.append(appElement("summary", "触发方式与角色（可选）"));
        const settings = value.reply_settings;
        const effective = settings || { mode: "mention", prefix: "", persona: "", force_reply_when_mentioned: false };
        const fields = appElement("div", "", "details-grid");
        const mode = document.createElement("select");
        mode.setAttribute("aria-label", threadID + " 群聊触发方式");
        for (const [optionValue, optionText] of [["mention", "只在提及时回复"], ["all", "每条群聊文字都交给大肥鱼判断"], ["social", "允许观察本群并自主选择接话"]]) {
          const option = document.createElement("option");
          option.value = optionValue;
          option.textContent = optionText;
          mode.append(option);
        }
        mode.value = effective.mode;
        const prefix = document.createElement("input");
        prefix.value = effective.prefix;
        prefix.placeholder = "可选触发前缀，保留末尾空格";
        prefix.setAttribute("aria-label", threadID + " 群聊触发前缀");
        const persona = document.createElement("textarea");
        persona.rows = 2;
        persona.value = effective.persona;
        persona.placeholder = "本群补充设定（可选）";
        persona.setAttribute("aria-label", threadID + " 群聊角色补充设定");
        const force = document.createElement("input");
        force.type = "checkbox";
        force.checked = effective.force_reply_when_mentioned;
        force.setAttribute("aria-label", threadID + " 被提及时强制回复");
        const writeSettings = () => updateGroupRule(threadID, { reply_settings: {
          mode: ["all", "social"].includes(mode.value) ? mode.value : "mention",
          prefix: prefix.value,
          persona: persona.value,
          force_reply_when_mentioned: force.checked
        } });
        mode.addEventListener("change", writeSettings);
        prefix.addEventListener("input", writeSettings);
        persona.addEventListener("input", writeSettings);
        force.addEventListener("change", writeSettings);
        const modeLabel = appElement("label", "回复触发方式");
        modeLabel.append(mode);
        const prefixLabel = appElement("label", "触发前缀");
        prefixLabel.append(prefix);
        const personaLabel = appElement("label", "本群补充设定");
        personaLabel.append(persona);
        const forceLabel = appElement("label", "", "switch-row");
        forceLabel.append(force, appElement("span", "被提及时强制回复"));
        fields.append(modeLabel, prefixLabel, personaLabel, forceLabel);
        if (settings) {
          const reset = appSetupButton("恢复默认触发方式", () => {
            updateGroupRule(threadID, { reply_settings: { mode: "mention" } });
            renderGroupRules();
          });
          fields.append(reset);
        }
        advanced.append(fields);
        row.append(advanced);
        groupRules.append(row);
      }
    };
    const attachmentThreadIDs = () => attachments.value.split(/[\s，,]+/u).filter(Boolean);
    const syncObservationOptions = () => {
      const noAttachmentScope = attachmentThreadIDs().length === 0;
      autoImages.disabled = noAttachmentScope;
      autoVideos.disabled = noAttachmentScope;
    };
    field.addEventListener("input", renderGroupRules);
    attachments.addEventListener("input", renderGroupRules);
    attachments.addEventListener("input", syncObservationOptions);
    renderGroupRules();
    syncObservationOptions();
    const save = appSetupButton("保存回复范围", () => {
      const reply_threads = field.value.split(/[\s，,]+/u).filter(Boolean);
      const history_threads = history.value.split(/[\s，,]+/u).filter(Boolean);
      const attachment_threads = attachmentThreadIDs();
      const revokedAttachmentThreads = new Set((status.configured_policy?.attachment_threads || []).filter((threadID) => !attachment_threads.includes(threadID)));
      const media_reply_threads = mediaReplies.value.split(/[\s，,]+/u).filter((threadID) => threadID && !revokedAttachmentThreads.has(threadID));
      const group_mute_threads = groupMutes.value.split(/[\s，,]+/u).filter(Boolean);
      const group_rules = reply_threads.filter((threadID) => threadID.endsWith("@chatroom")).map((thread_id) => {
        const rule = readGroupRule(thread_id);
        const payload = { thread_id, note: rule.note.trim(), summarize_shares: Boolean(rule.summarize_shares) && attachment_threads.includes(thread_id) };
        if (rule.reply_settings !== undefined) payload.reply_settings = rule.reply_settings;
        return payload;
      });
      runWorkerAction(id, "PUT", "policy", { reply_threads, history_threads, attachment_threads, auto_observe_images: attachment_threads.length > 0 && autoImages.checked, auto_observe_videos: attachment_threads.length > 0 && autoVideos.checked, media_reply_threads, contact_read: contactRead.checked, group_mute_threads, group_rules });
    });
    panel.append(appElement("p", "每行一个会话 ID。群聊默认在被提及时回复。"), field,
      appElement("p", "历史消息与群成员须单独授权；大肥鱼仅能读取当前所在会话。"), history,
      appElement("p", "附件读取须在回复范围内单独开启，原件保存在宿主私有目录。"), attachments, autoImagesLabel, autoVideosLabel,
      groupRules,
      appElement("p", "附件回复须单独授权，只能把附件发回原会话。撤销附件读取时同时撤销附件回复。"), mediaReplies,
      contactReadLabel,
      appElement("p", "仅允许大肥鱼在本地聊天中设置这些群的免打扰或取消免打扰；微信群聊与私聊中的 Agent 不会获得此项权限。"), groupMutes,
      save, workGrantControls(id));
    return panel;
  }
  function contactDirectoryControls(id) {
    const panel = appElement("details", "", "identity-details contact-directory");
    panel.append(appElement("summary", "通讯录"));
    const form = appElement("form", "", "details-grid");
    const search = document.createElement("input");
    search.placeholder = "昵称或微信 ID";
    search.setAttribute("aria-label", "搜索联系人");
    const limit = document.createElement("input");
    limit.type = "number";
    limit.min = "1";
    limit.step = "1";
    limit.value = "50";
    limit.required = true;
    limit.setAttribute("aria-label", "联系人每页数量");
    const searchLabel = appElement("label", "搜索联系人");
    searchLabel.append(search);
    const limitLabel = appElement("label", "每页数量");
    limitLabel.append(limit);
    form.append(searchLabel, limitLabel);
    const actions = appElement("div", "", "app-worker-actions");
    const query = appSetupButton("查询联系人");
    query.type = "submit";
    const next = appSetupButton("下一页", () => load(false));
    next.disabled = true;
    actions.append(query, next);
    form.append(actions);
    const note = appElement("small", "本页仅显示当前微信连接的联系人目录。");
    const records = appElement("div", "", "work-records");
    panel.append(form, note, records);
    let cursor = "", hasNext = false, busy = false, appliedSearch = "", appliedLimit = 50;
    async function load(reset) {
      if (busy) return;
      const searchValue = reset ? search.value.trim() : appliedSearch;
      const limitValue = reset ? limit.valueAsNumber : appliedLimit;
      if (!Number.isSafeInteger(limitValue) || limitValue < 1) {
        appFeedback("每页数量须为正整数。", true);
        return;
      }
      const requestCursor = reset ? "" : cursor;
      busy = true;
      query.disabled = next.disabled = true;
      try {
        const params = new URLSearchParams({ search: searchValue, limit: String(limitValue) });
        if (requestCursor) params.set("cursor", requestCursor);
        const result = await request("/api/apps/wechat/connections/" + encodeURIComponent(id) + "/contacts?" + params.toString());
        if (reset) { appliedSearch = searchValue; appliedLimit = limitValue; }
        records.replaceChildren();
        const items = result.items || [];
        for (const item of items) {
          records.append(appElement("div", (item.display_name || "未命名联系人") + " · " + item.id, "work-record"));
        }
        if (!records.children.length) records.append(appElement("small", "没有匹配的联系人。"));
        cursor = result.next_cursor || "";
        hasNext = Boolean(result.has_more && cursor);
        next.disabled = !hasNext;
        const effectiveLimit = Number.isInteger(result.effective_limit) && result.effective_limit > 0 ? result.effective_limit : limitValue;
        note.textContent = (result.coverage || "已读取当前可见联系人。") + " · 本页 " + items.length + " 条，每页最多 " + effectiveLimit + " 条" + (result.limit_reason ? " · " + result.limit_reason : "");
      } catch (error) {
        appFeedback(error.message, true);
      } finally {
        busy = false;
        query.disabled = false;
        next.disabled = !hasNext;
      }
    }
    form.addEventListener("submit", (event) => { event.preventDefault(); load(true); });
    return panel;
  }
  function groupDirectoryControls(id) {
    const panel = appElement("details", "", "identity-details group-directory");
    panel.append(appElement("summary", "群聊"));
    const form = appElement("form", "", "details-grid");
    const search = document.createElement("input");
    search.placeholder = "群名或群 ID";
    search.setAttribute("aria-label", "搜索群聊");
    const limit = document.createElement("input");
    limit.type = "number";
    limit.min = "1";
    limit.step = "1";
    limit.value = "50";
    limit.required = true;
    limit.setAttribute("aria-label", "群聊每页数量");
    const searchLabel = appElement("label", "搜索群聊");
    searchLabel.append(search);
    const limitLabel = appElement("label", "每页数量");
    limitLabel.append(limit);
    form.append(searchLabel, limitLabel);
    const actions = appElement("div", "", "app-worker-actions");
    const query = appSetupButton("查询群聊");
    query.type = "submit";
    const next = appSetupButton("下一页", () => load(false));
    next.disabled = true;
    actions.append(query, next);
    form.append(actions);
    const note = appElement("small", "本页仅显示当前微信连接可管理的群聊。");
    const records = appElement("div", "", "work-records");
    const control = appElement("div", "", "details-grid");
    control.hidden = true;
    panel.append(form, note, records, control);
    let cursor = "", hasNext = false, busy = false, appliedSearch = "", appliedLimit = 50;
    let selected = null, notification = null, profile = null, selectedAction = null, renameDraft = null, myNameDraft = null, controlBusy = false, needsRefresh = false, renameNeedsRefresh = false, myNameNeedsRefresh = false;
    const base = "/api/apps/wechat/connections/" + encodeURIComponent(id);
    function receiptLabel(operation) {
      if (!operation || !operation.state) return "尚无操作记录";
      if (["succeeded", "confirmed", "completed"].includes(operation.state)) return "已确认完成";
      if (["submitted_pending", "pending", "running"].includes(operation.state)) return "已提交，等待微信确认";
      if (["submission_unknown", "unknown"].includes(operation.state)) return "提交结果待核对";
      if (["cancelled", "canceled"].includes(operation.state)) return "操作已取消";
      if (["failed", "rejected"].includes(operation.state)) return "操作未完成";
      return "状态待核对";
    }
    function isOutstanding(operation) {
      return Boolean(operation && ["submitted_pending", "pending", "running", "submission_unknown", "unknown"].includes(operation.state));
    }
    function renderControl() {
      control.replaceChildren();
      if (!selected) { control.hidden = true; return; }
      control.hidden = false;
      control.append(appElement("strong", selected.display_name || "未命名群聊"), appElement("small", selected.id));
      const mutedKnown = typeof notification?.muted === "boolean";
      const coverage = notification?.coverage === "cached" ? "（来自本机缓存）" : "";
      control.append(appElement("small", mutedKnown
        ? (notification.muted ? "当前为免打扰" : "当前为正常提醒") + coverage
        : "当前免打扰状态未取得，可刷新后再决定。"));
      const operation = notification?.operation || null;
      if (operation) control.append(appElement("small", receiptLabel(operation) + (operation.operation_id ? " · 操作 " + operation.operation_id : "")));
      const refresh = appSetupButton(controlBusy ? "正在刷新…" : "刷新状态", () => loadNotification());
      refresh.disabled ||= controlBusy;
      control.append(refresh);
      if (needsRefresh) {
        control.append(appElement("small", "免打扰操作暂不可确认，请先刷新状态；不会自动重复提交。"));
      } else if (notification?.can_change !== true) {
        control.append(appElement("small", "当前连接不能修改这个群的免打扰；仍可刷新查看已有操作回执。"));
      } else if (isOutstanding(operation)) {
        control.append(appElement("small", "这项免打扰操作仍在核对中；刷新只查询状态，不会重复提交。"));
      } else {
        const choices = appElement("div", "", "app-worker-actions");
        const mute = appSetupButton("设为免打扰", () => { selectedAction = true; renderControl(); });
        const unmute = appSetupButton("取消免打扰", () => { selectedAction = false; renderControl(); });
        mute.disabled ||= controlBusy;
        unmute.disabled ||= controlBusy;
        choices.append(mute, unmute);
        control.append(choices);
        if (typeof selectedAction === "boolean") {
          const confirmation = appElement("div", "", "app-worker-actions");
          confirmation.append(appElement("small", `将“${selected.display_name || selected.id}”${selectedAction ? "设为免打扰" : "取消免打扰"}。`));
          const confirm = appSetupButton("确认提交", () => submitAction());
          const cancel = appSetupButton("取消", () => { selectedAction = null; renderControl(); });
          confirm.disabled ||= controlBusy;
          cancel.disabled ||= controlBusy;
          confirmation.append(confirm, cancel);
          control.append(confirmation);
        }
      }
      const renameOperation = profile?.operation || null;
      const myNameOperation = profile?.my_name_operation || null;
      if (!profile || !(profile.can_rename === true || renameOperation || profile.can_set_my_name === true || myNameOperation)) return;
      const profileSection = appElement("div", "", "details-grid");
      const refreshProfile = appSetupButton(controlBusy ? "正在刷新…" : "刷新群资料", () => loadProfile());
      refreshProfile.disabled ||= controlBusy;
      profileSection.append(refreshProfile);
      function appendTextAction(options) {
        const section = appElement("div", "", "details-grid");
        section.append(appElement("strong", options.title), appElement("small", options.currentLabel));
        if (options.operation) section.append(appElement("small", receiptLabel(options.operation) + (options.operation.operation_id ? " · 操作 " + options.operation.operation_id : "")));
        if (options.needsRefresh()) {
          section.append(appElement("small", options.retryMessage));
        } else if (isOutstanding(options.operation)) {
          section.append(appElement("small", options.pendingMessage));
        } else if (options.canChange === true) {
          const start = appSetupButton(options.startLabel, () => { options.setDraft(options.currentValue); renderControl(); });
          start.disabled ||= controlBusy;
          section.append(start);
          if (options.draft() !== null) {
            const input = document.createElement("input");
            input.value = options.draft();
            input.setAttribute("aria-label", options.inputLabel);
            const confirmation = appElement("div", "", "app-worker-actions");
            const confirmationCopy = appElement("small", options.confirmation(input.value));
            input.addEventListener("input", () => {
              options.setDraft(input.value);
              confirmationCopy.textContent = options.confirmation(input.value);
            });
            confirmation.append(input, confirmationCopy);
            const confirm = appSetupButton(options.confirmLabel, () => options.submit());
            const cancel = appSetupButton(options.cancelLabel, () => { options.setDraft(null); renderControl(); });
            confirm.disabled ||= controlBusy;
            cancel.disabled ||= controlBusy;
            confirmation.append(confirm, cancel);
            section.append(confirmation);
          }
        } else {
          section.append(appElement("small", options.unavailableMessage));
        }
        profileSection.append(section);
      }
      const name = typeof profile.name === "string" ? profile.name : "";
      appendTextAction({
        title: "群名称", currentValue: name,
        currentLabel: (name || "群名称尚未取得") + (name && profile.coverage === "cached" ? "（来自本机缓存）" : ""),
        operation: renameOperation, canChange: profile.can_rename, draft: () => renameDraft, setDraft: (value) => { renameDraft = value; },
        needsRefresh: () => renameNeedsRefresh, retryMessage: "改名结果暂不可确认，请先刷新群资料；不会自动重复提交。",
        pendingMessage: "这次改名仍在核对中；刷新只查询原操作，不会重复提交。",
        startLabel: "修改群名称", inputLabel: "新的群名称", confirmLabel: "确认改名", cancelLabel: "取消改名",
        confirmation: (value) => `将“${selected.display_name || selected.id}”改为“${value || "新名称"}”。`,
        unavailableMessage: "当前连接不能修改这个群名称；仍可刷新查看已有操作回执。", submit: () => submitRename()
      });
      const hasMyName = Object.prototype.hasOwnProperty.call(profile, "my_display_name");
      const myName = typeof profile.my_display_name === "string" ? profile.my_display_name : "";
      appendTextAction({
        title: "我的群昵称", currentValue: myName,
        currentLabel: hasMyName ? ((myName || "未设置") + (profile.coverage === "cached" ? "（来自本机缓存）" : "")) : "群昵称尚未取得",
        operation: myNameOperation, canChange: profile.can_set_my_name, draft: () => myNameDraft, setDraft: (value) => { myNameDraft = value; },
        needsRefresh: () => myNameNeedsRefresh, retryMessage: "群昵称修改结果暂不可确认，请先刷新群资料；不会自动重复提交。",
        pendingMessage: "这次群昵称修改仍在核对中；刷新只查询原操作，不会重复提交。",
        startLabel: "修改我的群昵称", inputLabel: "新的群昵称", confirmLabel: "确认修改昵称", cancelLabel: "取消修改昵称",
        confirmation: (value) => value === "" ? `将“${selected.display_name || selected.id}”中的我的群昵称清空。` : `将“${selected.display_name || selected.id}”中的我的群昵称改为“${value}”。`,
        unavailableMessage: "当前连接不能修改我的群昵称；仍可刷新查看已有操作回执。", submit: () => submitMyName()
      });
      control.append(profileSection);
    }
    async function loadNotification() {
      if (!selected || controlBusy) return;
      controlBusy = true;
      renderControl();
      try {
        notification = await request(base + "/groups/" + encodeURIComponent(selected.id) + "/notification");
        selectedAction = null;
        needsRefresh = false;
      } catch (error) {
        appFeedback(error.message, true);
      } finally {
        controlBusy = false;
        renderControl();
      }
    }
    async function loadProfile() {
      if (!selected || controlBusy) return;
      controlBusy = true;
      renderControl();
      try {
        profile = await request(base + "/groups/" + encodeURIComponent(selected.id) + "/profile");
        renameDraft = null;
        renameNeedsRefresh = false;
        myNameDraft = null;
        myNameNeedsRefresh = false;
      } catch (error) {
        appFeedback(error.message, true);
      } finally {
        controlBusy = false;
        renderControl();
      }
    }
    async function selectGroup(item) {
      if (controlBusy) return;
      selected = item;
      notification = null;
      profile = null;
      selectedAction = null;
      renameDraft = null;
      myNameDraft = null;
      needsRefresh = false;
      renameNeedsRefresh = false;
      myNameNeedsRefresh = false;
      renderControl();
      await loadNotification();
      await loadProfile();
    }
    async function submitAction() {
      if (!selected || typeof selectedAction !== "boolean" || controlBusy) return;
      controlBusy = true;
      renderControl();
      try {
        const receipt = await request(base + "/group-actions", { method: "POST", body: JSON.stringify({
          group_id: selected.id, muted: selectedAction, request_key: crypto.randomUUID()
        }) });
        notification = { ...(notification || {}), operation: receipt };
        selectedAction = null;
      } catch (error) {
        selectedAction = null;
        needsRefresh = true;
        appFeedback(error.message, true);
      } finally {
        controlBusy = false;
        renderControl();
      }
    }
    async function submitRename() {
      if (!selected || controlBusy) return;
      const name = renameDraft || "";
      if (!name.trim() || new TextEncoder().encode(name).length > 512 || /\p{Cc}/u.test(name)) {
        appFeedback("新群名称须非空、最多 512 UTF-8 字节，且不能含控制字符。", true);
        return;
      }
      controlBusy = true;
      renderControl();
      try {
        const receipt = await request(base + "/group-actions", { method: "POST", body: JSON.stringify({
          action: "rename", group_id: selected.id, name, request_key: crypto.randomUUID()
        }) });
        profile = { ...(profile || {}), operation: receipt };
        renameDraft = null;
      } catch (error) {
        renameDraft = null;
        renameNeedsRefresh = true;
        appFeedback(error.message, true);
      } finally {
        controlBusy = false;
        renderControl();
      }
    }
    async function submitMyName() {
      if (!selected || controlBusy) return;
      const name = myNameDraft === null ? "" : myNameDraft;
      if (new TextEncoder().encode(name).length > 512 || /\p{Cc}/u.test(name)) {
        appFeedback("群昵称最多 512 UTF-8 字节，且不能含控制字符。", true);
        return;
      }
      controlBusy = true;
      renderControl();
      try {
        const receipt = await request(base + "/group-actions", { method: "POST", body: JSON.stringify({
          action: "my_name", group_id: selected.id, name, request_key: crypto.randomUUID()
        }) });
        profile = { ...(profile || {}), my_name_operation: receipt };
        myNameDraft = null;
      } catch (error) {
        myNameDraft = null;
        myNameNeedsRefresh = true;
        appFeedback(error.message, true);
      } finally {
        controlBusy = false;
        renderControl();
      }
    }
    async function load(reset) {
      if (busy) return;
      const searchValue = reset ? search.value.trim() : appliedSearch;
      const limitValue = reset ? limit.valueAsNumber : appliedLimit;
      if (!Number.isSafeInteger(limitValue) || limitValue < 1) {
        appFeedback("每页数量须为正整数。", true);
        return;
      }
      const requestCursor = reset ? "" : cursor;
      busy = true;
      query.disabled = next.disabled = true;
      try {
        const params = new URLSearchParams({ search: searchValue, limit: String(limitValue) });
        if (requestCursor) params.set("cursor", requestCursor);
        const result = await request(base + "/groups?" + params.toString());
        if (reset) { appliedSearch = searchValue; appliedLimit = limitValue; }
        records.replaceChildren();
        const items = result.items || [];
        for (const item of items) {
          const row = appSetupButton((item.display_name || "未命名群聊") + " · " + item.id, () => selectGroup(item));
          row.classList.add("group-directory-row");
          records.append(row);
        }
        if (!records.children.length) records.append(appElement("small", "没有匹配的群聊。"));
        cursor = result.next_cursor || "";
        hasNext = Boolean(result.has_more && cursor);
        next.disabled = !hasNext;
        const effectiveLimit = Number.isInteger(result.effective_limit) && result.effective_limit > 0 ? result.effective_limit : limitValue;
        note.textContent = (result.coverage || "已读取当前可见群聊。") + " · 本页 " + items.length + " 条，每页最多 " + effectiveLimit + " 条" + (result.limit_reason ? " · " + result.limit_reason : "");
      } catch (error) {
        appFeedback(error.message, true);
      } finally {
        busy = false;
        query.disabled = false;
        next.disabled = !hasNext;
      }
    }
    form.addEventListener("submit", (event) => { event.preventDefault(); load(true); });
    return panel;
  }
  function stationToolControls(id, media) {
    const name = media ? "媒体理解" : "视频号解析";
    const tool = media ? "media-understanding" : "share-reader";
    const fieldsSpec = media ? [
      ["vision_endpoint", "视觉接口地址"], ["vision_model", "视觉模型"], ["vision_key", "视觉接口密钥", true],
      ["asr_endpoint", "语音识别接口地址"], ["asr_model", "语音识别模型"], ["asr_key", "语音识别接口密钥", true]
    ] : [
      ["yuanbao_cookie", "元宝解析凭据", true], ["assistant_cookie", "视频号助手凭据", true], ["object_bridge_url", "本机对象桥接地址"]
    ];
    const panel = appElement("details", "", "identity-details");
    panel.append(appElement("summary", name));
    const content = appElement("div", "", "details-grid");
    panel.append(content);
    const path = "/api/apps/wechat/connections/" + encodeURIComponent(id) + "/" + tool;
    let busy = false;
    async function load(body) {
      if (busy) return;
      busy = true;
      content.querySelectorAll("button").forEach((button) => { button.disabled = true; });
      try {
        const value = await request(path, body ? { method: "PUT", body: JSON.stringify(body) } : undefined);
        render(value);
      } catch (error) {
        content.querySelectorAll("button").forEach((button) => { button.disabled = false; });
        let errorRow = content.querySelector(".share-reader-error");
        if (!errorRow) {
          errorRow = appElement("small", "", "share-reader-error");
          content.append(errorRow);
        }
        errorRow.textContent = error.message;
      } finally { busy = false; }
    }
    function render(value) {
      let status = value.installed ? (value.enabled ? "已启用，按需运行" : "已停用") : "尚未安装";
      if (media && value.enabled && value.local_asr_ready === false) status = "已启用 · 本机识别未就绪";
      content.replaceChildren(appElement("small", value.runtime_ready ? status : `${status} · 缺少 Windows 隔离工具运行环境`));
      if (media && value.local_asr_ready === false) content.append(appElement("small", "请先检查项目工具中的本地媒体识别资源。", "share-reader-error"));
      if (!value.installed) {
        const explanation = media ? "仅处理当前连接已授权的视频。实际帧或音轨通过宿主代理发送到配置的模型接口。" : "授权后，只为当前微信连接解析原卡片。";
        const approval = media ? { grant_media: true } : { grant_network: true };
        content.append(appElement("small", explanation),
          appSetupButton(media ? "安装并授权媒体处理和模型调用" : "安装并授予受控联网权限", () => load({ action: "install", ...approval })));
        return;
      }
      const action = value.enabled ? "disable" : "enable";
      content.append(appSetupButton((value.enabled ? "停用" : "启用") + (media ? "媒体理解" : "解析"), () => load({ action, revision: value.revision })));
      if (value.enabled) return;
      if (media) content.append(appElement("small", "视觉与音轨转写分别配置。接口地址须为完整 HTTPS 地址。"));
      const configuration = value.configuration || {};
      const fields = new Map();
      const clear = new Map();
      const asrRows = [];
      let asrBackend = null, localASRNote = null;
      if (media) {
        const wrap = appElement("label", "语音识别方式", "details-grid");
        asrBackend = document.createElement("select");
        asrBackend.setAttribute("aria-label", "语音识别方式");
        asrBackend.append(new Option("远程接口", "remote"), new Option("本机识别", "local"));
        asrBackend.value = configuration.values?.asr_backend === "local" ? "local" : "remote";
        wrap.append(asrBackend);
        localASRNote = appElement("small", "使用主人已登记的本地媒体识别；不会改用远程接口。");
        content.append(wrap, localASRNote);
      }
      for (const [key, label, secret] of fieldsSpec) {
        const wrap = appElement("label", label, "details-grid");
        const field = document.createElement("input");
        field.type = secret ? "password" : "text";
        field.autocomplete = secret ? "new-password" : "off";
        field.value = secret ? "" : configuration.values?.[key] || "";
        field.placeholder = secret && configuration.configured_secrets?.includes(key) ? "已配置，留空保留" : "可留空";
        fields.set(key, field);
        wrap.append(field);
        content.append(wrap);
        if (media && key.startsWith("asr_")) asrRows.push(wrap);
        if (secret && configuration.configured_secrets?.includes(key)) {
          const clearLabel = appElement("label", "清除已保存的" + label);
          const checkbox = document.createElement("input");
          checkbox.type = "checkbox";
          clear.set(key, checkbox);
          clearLabel.prepend(checkbox);
          content.append(clearLabel);
          if (media && key.startsWith("asr_")) asrRows.push(clearLabel);
        }
      }
      if (asrBackend) {
        const syncASRBackend = () => {
          const local = asrBackend.value === "local";
          asrRows.forEach((row) => { row.hidden = local; });
          localASRNote.hidden = !local;
        };
        asrBackend.addEventListener("change", syncASRBackend);
        syncASRBackend();
      }
      content.append(appSetupButton(media ? "保存媒体设置" : "保存解析设置", () => {
        const values = {};
        if (media) {
          for (const key of ["vision_backend", "thinking", "reasoning_effort", "max_tokens"]) {
            if (configuration.values?.[key] !== undefined) values[key] = configuration.values[key];
          }
          values.asr_backend = asrBackend?.value === "local" ? "local" : "remote";
        }
        const secrets = {};
        const clear_secrets = [];
        for (const [key, , secret] of fieldsSpec) {
          const field = fields.get(key);
          if (secret) {
            if (clear.get(key)?.checked) clear_secrets.push(key);
            else if (field.value) secrets[key] = field.value;
          } else if (field.value.trim()) values[key] = field.value.trim();
        }
        load({ action: "configure", revision: value.revision, configuration: {
          revision: configuration.revision || 0, values, secrets, clear_secrets
        } });
      }));
    }
    panel.addEventListener("toggle", () => { if (panel.open) load(); });
    return panel;
  }
  function workGrantControls(id) {
    const panel = appElement("details", "", "identity-details work-grant-panel");
    panel.append(appElement("summary", "授权作品交付"));
    const form = appElement("form", "", "details-grid");
    const draft = apps.workDrafts.get(id) || { artifact_id: "", thread_id: "", requester_id: "", request_key: crypto.randomUUID() };
    apps.workDrafts.set(id, draft);
    for (const [key, label, placeholder] of [["artifact_id", "作品编号", "登记作品后得到的 work- 编号"], ["thread_id", "接收会话", "已允许附件回复的会话 ID"], ["requester_id", "请求者", "该会话中请求作品的微信 ID"]]) {
      const wrap = appElement("label", label, "details-grid");
      const input = document.createElement("input");
      input.value = draft[key]; input.required = true; input.placeholder = placeholder;
      input.addEventListener("input", () => { draft[key] = input.value; });
      wrap.append(input); form.append(wrap);
    }
    const actions = appElement("div", "", "app-worker-actions");
    const grant = appElement("button", "允许交付", "button compact secondary");
    grant.type = "submit";
    const revoke = appSetupButton("撤销此授权", () => save(false));
    actions.append(grant, revoke);
    form.append(actions, appElement("small", "授权后，大肥鱼可应此人的请求发送该作品。撤销时会暂停此连接。"));
    async function save(enabled) {
      if (!form.reportValidity()) return;
      grant.disabled = revoke.disabled = true;
      try {
        await request("/api/apps/wechat/connections/" + encodeURIComponent(id) + "/work-grants", { method: "POST", body: JSON.stringify({ ...draft, enabled }) });
        appFeedback(enabled ? "作品交付已授权，可在指定会话向大肥鱼索取。" : "作品交付已撤销，连接已暂停。");
        if (!enabled) { draft.request_key = crypto.randomUUID(); await loadApps(); }
      } catch (error) { appFeedback(error.message, true); }
      finally { grant.disabled = revoke.disabled = false; }
    }
    form.addEventListener("submit", (event) => { event.preventDefault(); save(true); });
    panel.append(form);
    const records = appElement("div", "", "work-records");
    let after = "", loaded = false;
    const list = appSetupButton("查看已有授权", async () => {
      list.disabled = true;
      try {
        const result = await request("/api/apps/wechat/connections/" + encodeURIComponent(id) + "/work-grants?limit=20&after=" + encodeURIComponent(after));
        if (!loaded) records.replaceChildren();
        for (const item of result.items || []) {
          const row = appElement("div", "", "work-record");
          const status = !item.enabled ? "已撤销" : item.usable === false ? "来源或范围已变化" : "已授权";
          const text = appElement("small", (item.filename ? item.filename + " · " : "") + item.thread_id + " · " + item.requester_id + " · " + status);
          const remove = appSetupButton("撤销", async () => {
            remove.disabled = true;
            try {
              await request("/api/apps/wechat/connections/" + encodeURIComponent(id) + "/work-grants/" + encodeURIComponent(item.delivery_id) + "/revoke", { method: "POST", body: "{}" });
              text.textContent = item.thread_id + " · 已撤销，连接已暂停";
            } catch (error) { appFeedback(error.message, true); remove.disabled = false; }
          });
          remove.disabled = !item.enabled;
          row.append(text, remove); records.append(row);
        }
        if (!records.children.length) records.append(appElement("small", "还没有作品交付授权。"));
        after = result.next_after || ""; loaded = true;
        list.textContent = after ? "继续查看" : "已列出全部";
        list.disabled = !after;
      } catch (error) { appFeedback(error.message, true); list.disabled = false; }
    });
    panel.append(list, records);
    return panel;
  }
  function deliveryRecords(id) {
    const panel = appElement("details", "", "identity-details delivery-records");
    panel.append(appElement("summary", "投递记录"));
    const records = appElement("div", "", "work-records");
    const note = appElement("small", "核对只查询原任务。停止作用于本回合的剩余投递，已接受的消息保留。");
    const base = "/api/apps/wechat/connections/" + encodeURIComponent(id) + "/receipts";
    let after = "", snapshot = "", loading = false;
    function describe(item) {
      const labels = { accepted: "微信已接受", not_submitted: "待发送", submitted_pending: "等待微信回执", submission_unknown: "发送结果待核对", failed: "发送失败", cancelled: "已取消" };
      const kinds = { text: "文字", voice: "语音", image: "图片", video: "视频", file: "文件" };
      let status = labels[item.state] || "状态待核对";
      const accepted = item.accepted_parts?.length || 0;
      const pending = item.unconfirmed_parts?.filter((part) => part.state !== "cancelled").length || 0;
      const cancelled = item.unconfirmed_parts?.filter((part) => part.state === "cancelled").length || 0;
      if ((item.cancel_requested || item.state === "cancelled") && item.state !== "accepted") {
        status = item.state === "cancelled" && !pending && !item.parts_incomplete ? "已取消" : "正在停止，等待回执";
      }
      const parts = [];
      if (accepted) parts.push(`已接受 ${accepted} 段`);
      if (pending) parts.push(`待核对 ${pending} 段`);
      if (cancelled) parts.push(`已取消 ${cancelled} 段`);
      if (item.parts_incomplete) parts.push("分段记录尚不完整");
      return (kinds[item.kind] || "消息") + (item.resumed_from ? "续发" : "") + " · " + item.conversation_id + " · " + status + (parts.length ? "（" + parts.join("，") + "）" : "") + (item.continuation?.message ? " · " + item.continuation.message : "");
    }
    function append(item) {
      const row = appElement("div", "", "work-record");
      const text = appElement("small", describe(item));
      const recall = appElement("div", "", "delivery-recall");
      const recallState = { serverID: "", cursor: "", messageID: "", operation: null, submitting: false, lookingUp: false, checked: false };
      function acceptedServers(receipt) {
        const seen = new Set();
        const add = (serverID, label) => {
          const value = String(serverID || "").trim();
          if (!value || seen.has(value)) return;
          seen.add(value);
          servers.push({ value, label });
        };
        const servers = [];
        for (const part of receipt.accepted_parts || []) add(part.server_id, `已接受第 ${(Number(part.index) || 0) + 1} 段`);
        if (receipt.state === "accepted") add(receipt.server_id, "已接受消息");
        return servers;
      }
      function operationText(operation) {
        if (!operation) return "";
        const labels = { succeeded: "撤回已完成", failed: "撤回失败", rejected: "撤回被拒绝", cancelled: "撤回已取消", submitted_pending: "撤回请求已提交，等待微信核对", waiting_receipt: "撤回请求已提交，等待微信核对", tracking: "正在核对撤回结果", unknown: "撤回结果待核对", submission_unknown: "撤回结果待核对" };
        return labels[operation.state] || "撤回状态待核对";
      }
      function operationFinal(operation) {
        return ["succeeded", "failed", "rejected", "cancelled"].includes(operation?.state);
      }
      function operationNeedsLookup(operation) {
        return ["submitted_pending", "waiting_receipt", "tracking", "unknown", "submission_unknown"].includes(operation?.state);
      }
      function resultOperation(result) {
        return result?.operation || (result?.operation_id ? result : null);
      }
      async function lookupRecall(cursor = "") {
        const serverID = recallState.serverID;
        const query = new URLSearchParams({ server_id: serverID });
        if (cursor) query.set("cursor", cursor);
        recallState.lookingUp = true;
        renderRecall();
        try {
          const result = await request(base + "/" + encodeURIComponent(item.idempotency_key) + "/recall?" + query.toString());
          // A selector change must never attach an old part's lookup result to a new part.
          if (serverID !== recallState.serverID) return result;
          recallState.checked = true;
          recallState.cursor = result.next_cursor || "";
          recallState.messageID = result.message_id || "";
          const operation = resultOperation(result);
          if (operation) recallState.operation = operation;
          return result;
        } finally {
          if (serverID === recallState.serverID) {
            recallState.lookingUp = false;
            renderRecall();
          }
        }
      }
      function renderRecall() {
        recall.replaceChildren();
        const servers = acceptedServers(item);
        if (!servers.length) return;
        if (!servers.some((server) => server.value === recallState.serverID)) {
          recallState.serverID = servers[0].value;
          recallState.cursor = ""; recallState.messageID = ""; recallState.operation = null; recallState.submitting = false; recallState.lookingUp = false; recallState.checked = false;
        }
        const selected = servers.find((server) => server.value === recallState.serverID);
        if (servers.length > 1) {
          const select = document.createElement("select");
          select.setAttribute("aria-label", "选择要撤回的已确认消息");
          for (const server of servers) {
            const option = document.createElement("option"); option.value = server.value; option.textContent = server.label; option.selected = server.value === recallState.serverID;
            select.append(option);
          }
          select.disabled = recallState.submitting || recallState.lookingUp;
          select.addEventListener("change", () => {
            recallState.serverID = select.value; recallState.cursor = ""; recallState.messageID = ""; recallState.operation = null; recallState.submitting = false; recallState.lookingUp = false; recallState.checked = false;
            renderRecall();
          });
          recall.append(select);
        } else {
          recall.append(appElement("small", selected.label));
        }
        const blocked = operationNeedsLookup(recallState.operation) || recallState.submitting;
        if (recallState.operation) recall.append(appElement("small", operationText(recallState.operation)));
        if (recallState.lookingUp) {
          recall.append(appElement("small", "正在核对撤回记录"));
        } else if (operationFinal(recallState.operation)) {
          // A terminal receipt is authoritative. Never offer a second POST that could
          // turn a failed, rejected, or cancelled recall into an accidental retry.
        } else if (recallState.messageID && !blocked && !recallState.submitting) {
          const confirm = appSetupButton("确认撤回", async () => {
            const serverID = recallState.serverID, messageID = recallState.messageID;
            recallState.submitting = true; renderRecall();
            try {
              const posted = await request(base + "/" + encodeURIComponent(item.idempotency_key) + "/recall", { method: "POST", body: JSON.stringify({ server_id: serverID, message_id: messageID }) });
              if (serverID !== recallState.serverID) return;
              recallState.operation = resultOperation(posted) || { state: "submission_unknown" };
            } catch (error) {
              if (serverID !== recallState.serverID) return;
              recallState.operation = { state: "submission_unknown" };
              appFeedback(error.message, true);
            }
            try {
              await lookupRecall();
            } catch (error) {
              if (serverID === recallState.serverID) appFeedback(error.message, true);
            } finally {
              if (serverID === recallState.serverID) {
                recallState.submitting = false;
                renderRecall();
              }
            }
          });
          recall.append(confirm);
        } else if (recallState.cursor && !blocked) {
          const moreRecall = appSetupButton("继续查找", async () => {
            moreRecall.disabled = true; moreRecall.textContent = "正在查找…";
            try { await lookupRecall(recallState.cursor); } catch (error) { appFeedback(error.message, true); renderRecall(); }
          });
          recall.append(moreRecall);
        } else if (!recallState.checked || blocked) {
          const locate = appSetupButton(blocked ? "核对撤回状态" : "定位可撤回消息", async () => {
            locate.disabled = true; locate.textContent = "正在核对…";
            try { await lookupRecall(); } catch (error) { appFeedback(error.message, true); renderRecall(); }
          });
          recall.append(locate);
        } else {
          recall.append(appElement("small", "没有找到可撤回的本人消息"));
        }
      }
      const stop = appSetupButton("停止本回合投递", async () => {
        stop.disabled = true; stop.textContent = "正在停止…";
        try {
          await request(base + "/" + encodeURIComponent(item.idempotency_key) + "/cancel", { method: "POST", body: "{}" });
          item.cancel_requested = true;
          if (item.continuation) item.continuation = { ...item.continuation, state: "cancelled", message: "续办已停止" };
          text.textContent = describe(item);
          updateActions();
          stop.textContent = "停止意图已保存";
        } catch (error) { appFeedback(error.message, true); stop.disabled = false; stop.textContent = "停止本回合投递"; }
      });
      const resume = appSetupButton("续发未发送片段", async () => {
        resume.disabled = true; resume.textContent = "正在保存…";
        const draftID = id + "|" + item.idempotency_key;
        if (!apps.resumeDrafts.has(draftID)) apps.resumeDrafts.set(draftID, crypto.randomUUID());
        try {
          const result = await request(base + "/" + encodeURIComponent(item.idempotency_key) + "/resume", { method: "POST", body: JSON.stringify({ request_key: apps.resumeDrafts.get(draftID) }) });
          item.continuation = result.continuation;
          text.textContent = describe(item);
          updateActions();
        } catch (error) { appFeedback(error.message, true); updateActions(); }
      });
      function updateActions() {
        const active = ["requested", "waiting_receipt", "tracking"].includes(item.continuation?.state);
        stop.disabled = Boolean(!active && (item.cancel_requested || item.state === "cancelled"));
        stop.textContent = active ? "停止续办" : "停止本回合投递";
        resume.hidden = item.kind !== "voice" || !(item.cancel_requested || item.state === "cancelled");
        resume.disabled = Boolean(item.continuation || item.state === "accepted" || item.state === "cancelled" && !item.operation_id);
        resume.textContent = item.continuation ? "续办请求已保存" : "续发未发送片段";
      }
      const check = appSetupButton("核对回执", async () => {
        check.disabled = true; check.textContent = "正在核对…";
        try {
          const result = await request(base + "/" + encodeURIComponent(item.idempotency_key));
          item = result;
          text.textContent = describe(result);
          updateActions();
          renderRecall();
          if (result.lookup === "unavailable") text.textContent += " · 暂时无法连接，保留已知状态";
          const nextKey = result.continuation?.delivery_key;
          if (nextKey && nextKey !== result.idempotency_key) {
            try {
              const next = await request(base + "/" + encodeURIComponent(nextKey));
              if (next.resumed_from === result.idempotency_key && next.conversation_id === result.conversation_id) {
                text.textContent += " · 续发结果：" + describe(next);
              }
            } catch (_) { text.textContent += " · 续发回执暂不可读"; }
          }
        } catch (error) { appFeedback(error.message, true); }
        finally { check.disabled = false; check.textContent = "核对回执"; }
      });
      updateActions();
      renderRecall();
      row.append(text, check, stop, resume, recall); records.append(row);
    }
    const more = appSetupButton("查看投递记录", async () => {
      if (loading) return;
      loading = true; more.disabled = true; more.textContent = "正在读取…";
      try {
        const result = await request(base + "?limit=20&after=" + encodeURIComponent(after));
        if (after && snapshot !== (result.snapshot || "")) {
          after = ""; snapshot = "";
          note.textContent = "投递记录已更新，请重新查看。";
          more.textContent = "重新查看";
          return;
        }
        if (!after) records.replaceChildren();
        for (const item of result.items || []) append(item);
        if (!records.children.length) records.append(appElement("small", "此连接还没有当前投递记录。"));
        snapshot = result.snapshot || ""; after = result.next_after || "";
        note.textContent = "仅含当前连接队列保存的记录；旧版导入回执仍可按原编号查询。";
        more.textContent = after ? "继续查看" : "重新查看";
      } catch (error) { appFeedback(error.message, true); more.textContent = "重新查看"; }
      finally { loading = false; more.disabled = false; }
    });
    panel.append(note, records, more);
    return panel;
  }
  $("#artifact-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = event.currentTarget.querySelector("button"), path = $("#artifact-path").value;
    button.disabled = true;
    try {
      const artifact = await request("/api/local/artifacts", { method: "POST", body: JSON.stringify({ path, request_key: crypto.randomUUID() }) });
      $("#artifact-result").textContent = artifact.filename + " 已登记，作品编号 " + artifact.artifact_id;
    } catch (error) { $("#artifact-result").textContent = error.message; }
    finally { button.disabled = false; }
  });
  async function runWorkerAction(id, method, action, body) {
    const current = apps;
    if (current.setupBusy || current.loading) return;
    current.setupBusy = true;
    appFeedback("");
    renderApps();
    try {
      const path = "/api/apps/wechat/connections/" + encodeURIComponent(id) + "/" + action;
      const result = await request(path, { method, body: JSON.stringify(body) });
      if (current !== apps) return;
      if (action === "policy") {
        current.workerDrafts.delete(id);
        current.historyDrafts.delete(id);
        current.attachmentDrafts.delete(id);
        current.autoImagesDrafts.delete(id);
        current.autoVideosDrafts.delete(id);
        current.mediaReplyDrafts.delete(id);
        current.groupMuteDrafts.delete(id);
        current.groupRuleDrafts.delete(id);
        appFeedback(result.connection?.submissions_paused ? "回复范围已保存，Agent 已暂停。" : "回复范围已保存。");
      } else {
        appFeedback(body.action === "stop" ? "已暂停此账号的 Agent。" : "已启动此账号的 Agent，正在确认状态。");
      }
      await loadApps();
    } catch (error) {
      if (current === apps && !appSessionExpired([{ status: "rejected", reason: error }])) {
        appFeedback(error.message, true);
        if (error.status === 409) await loadApps();
      }
    } finally {
      current.setupBusy = false;
      if (current === apps) renderApps();
    }
  }
  function appConnectionRow(record) {
    const row = appElement("div", "", "app-layer");
    const detail = appElement("div", "");
    const connection = apps.connections.get(record.id);
    const canSetup = record.id === "wechat" && apps.registered?.some((app) => app.id === record.id && app.account_setup);
    if (!apps.registered || connection === null) {
      detail.append(appElement("strong", "账号状态暂不可读"), appElement("p", "可刷新后重查。"));
    } else if (!connection?.length) {
      const nativeReady = apps.registered.some((app) => app.id === record.id && app.registration === "station-native");
      detail.append(appElement("strong", "账号未接入"),
        appElement("p", nativeReady ? "原生服务已就绪，暂无已登记账号。" : "账号接入后会显示在这里。"));
    } else {
      detail.append(appElement("strong", `已有 ${connection.length} 个账号连接`));
      const list = appElement("ul", "", "app-accounts");
      for (const [index, account] of connection.entries()) {
        const item = appElement("li", "");
        item.append(appElement("span", account.label || `账号 ${index + 1}`));
        if (account.primary) item.append(appElement("small", "主连接"));
        const status = apps.accountStatuses.get(account.id);
        if (apps.accountStatuses.has(account.id)) {
          const login = status?.login === "authenticated" ? "已登录" : status?.login === "signed_out" ? "未登录" : "登录状态暂不可读";
          item.append(appElement("small", login));
        }
        const entry = apps.logins.get(account.id);
        if (canSetup && status?.login === "signed_out" && status.runtime !== "starting" && entry?.op?.state !== "succeeded") {
          const label = pendingAppLogin(entry?.op) ? "查看扫码" : "扫码登录";
          item.append(appSetupButton(label, () => runAppSetup(account.id)));
        }
        if (apps.selectedLogin === account.id && entry?.op && (status?.login !== "authenticated" || entry.op.state === "succeeded")) {
          item.append(renderAppLogin(account.id, entry.op));
        }
        if (canSetup && status && appWorkerLabels[status.worker]) item.append(appWorkerControls(account.id, status));
        list.append(item);
      }
      detail.append(list);
    }
    row.append(appElement("h5", "账号连接"), detail);
    if (canSetup && connection !== null) row.append(appSetupButton("添加账号", () => runAppSetup()));
    return row;
  }
  function validDouyinWebLogin(value) {
    return value && Number.isFinite(Number(value.revision)) &&
      ["unconfigured", "unconfirmed", "saved", "login_open"].includes(value.state) &&
      typeof value.active === "boolean" && typeof value.available === "boolean" &&
      (typeof value.attempt_id === "undefined" || typeof value.attempt_id === "string") &&
      (typeof value.confirmed_at === "undefined" || typeof value.confirmed_at === "string") &&
      (typeof value.read_enabled === "undefined" || typeof value.read_enabled === "boolean") &&
      (typeof value.read_scope === "undefined" || ["home", "home_and_videos"].includes(value.read_scope)) &&
      (typeof value.playback_enabled === "undefined" || typeof value.playback_enabled === "boolean") &&
      (typeof value.audio_enabled === "undefined" || typeof value.audio_enabled === "boolean") &&
      (typeof value.visual_enabled === "undefined" || typeof value.visual_enabled === "boolean") &&
      (typeof value.comments_enabled === "undefined" || typeof value.comments_enabled === "boolean") &&
      (typeof value.likes_enabled === "undefined" || typeof value.likes_enabled === "boolean") &&
      (typeof value.replies_enabled === "undefined" || typeof value.replies_enabled === "boolean") &&
      (typeof value.visual_ready === "undefined" || typeof value.visual_ready === "boolean") &&
      (typeof value.visual_reason === "undefined" || typeof value.visual_reason === "string") &&
      (typeof value.publish_enabled === "undefined" || typeof value.publish_enabled === "boolean") &&
      (typeof value.reason === "undefined" || typeof value.reason === "string");
  }
  function douyinWebReadEnabled(value) { return value?.read_enabled === true; }
  function douyinWebReadScope(value) { return value?.read_scope === "home_and_videos" ? "home_and_videos" : "home"; }
  function douyinWebPlaybackEnabled(value) { return douyinWebReadEnabled(value) && douyinWebReadScope(value) === "home_and_videos" && value?.playback_enabled === true; }
  function douyinWebAudioEnabled(value) { return douyinWebReadEnabled(value) && douyinWebReadScope(value) === "home_and_videos" && value?.audio_enabled === true; }
  function douyinWebVisualEnabled(value) { return douyinWebReadEnabled(value) && douyinWebReadScope(value) === "home_and_videos" && value?.visual_enabled === true; }
  function douyinWebCommentsEnabled(value) { return douyinWebReadEnabled(value) && douyinWebReadScope(value) === "home_and_videos" && value?.comments_enabled === true; }
  function douyinWebLikesEnabled(value) { return douyinWebReadEnabled(value) && douyinWebReadScope(value) === "home_and_videos" && value?.likes_enabled === true; }
  function douyinWebRepliesEnabled(value) { return douyinWebCommentsEnabled(value) && value?.replies_enabled === true; }
  function douyinWebPublishEnabled(value) { return value?.state === "saved" && value?.publish_enabled === true; }
  function stopDouyinWebLoginPolling(current = apps) {
    const web = current?.douyinWebLogin;
    if (!web) return;
    window.clearTimeout(web.poll);
    web.poll = 0;
    web.generation += 1;
    web.loading = false;
    web.applying = "";
  }
  function stopDouyinInteractionWork(current = apps) {
    const work = current?.douyinInteractions;
    if (!work) return;
    // Invalidate reads/actions from a closed or hidden app sheet. Keep the
    // visible page for a quick reopen. A submitted cancellation remains busy:
    // allowing a duplicate cancel could race the native worker. Other old
    // operations detach, so a late finally block cannot leave fresh controls
    // disabled after close/reopen.
    work.epoch += 1;
    work.readID += 1;
    work.reading = false;
    work.pending = new Map([...work.pending].filter(([, pending]) => pending.has("cancel")));
  }
  function shouldPollDouyinWebLogin(current, value) {
    return current === apps && activeView === "apps" && $("#app-dialog").open && current.openID === "douyin" &&
      current.detailTab === "account" && value?.active === true;
  }
  function scheduleDouyinWebLoginPoll(current) {
    const web = current?.douyinWebLogin;
    if (!web || !shouldPollDouyinWebLogin(current, web.value) || web.poll) return;
    const generation = web.generation;
    web.poll = window.setTimeout(() => {
      web.poll = 0;
      if (current === apps && web.generation === generation) void loadDouyinWebLogin(current);
    }, 1800);
  }
  async function loadDouyinWebLogin(current = apps) {
    const web = current?.douyinWebLogin;
    if (!web || current !== apps || web.loading || web.applying) return;
    const generation = web.generation;
    web.loading = true;
    try {
      const value = await request("/api/local/apps/douyin/web-login");
      if (current !== apps || web.generation !== generation) return;
      if (!validDouyinWebLogin(value)) throw new Error("网页登录状态返回格式无效，请刷新后重试。");
      web.value = value;
      web.loaded = true;
      web.error = "";
    } catch (error) {
      if (current !== apps || web.generation !== generation || appSessionExpired([{ status: "rejected", reason: error }])) return;
      web.error = error.message || "网页登录状态暂不可读。";
      web.loaded = true;
    } finally {
      if (current === apps && web.generation === generation) {
        web.loading = false;
        if (shouldPollDouyinWebLogin(current, web.value)) scheduleDouyinWebLoginPoll(current);
        renderApps();
      }
    }
  }
  function douyinWebLoginSummary(value) {
    const states = {
      unconfigured: ["尚未保存网页登录资料", "在独立网页窗口完成普通账号登录后，再回到这里保存。"],
      login_open: ["网页登录窗口已打开", "请在新窗口完成登录；保存前不会把它当作已登录。"],
      unconfirmed: ["网页登录资料待核对", "资料已保留，重新打开窗口核对登录后再保存。"],
      saved: ["网页登录资料已保存", "登录状态以网页为准，保存后可用于本软件已支持的抖音功能。"],
    };
    if (value?.state === "unconfirmed" && value.attempt_id) return ["等待主人确认", "请确认新窗口已完成登录，再保存资料。"];
    return states[value?.state] || ["网页登录状态暂不可读", "刷新后可重新查看。"];
  }
  async function updateDouyinWebLogin(current, action, options = {}) {
    const web = current?.douyinWebLogin, value = web?.value;
    if (!web || !value || current !== apps || web.applying || !Number.isFinite(Number(value.revision))) return;
    if ((action === "confirm" || action === "cancel") && !value.attempt_id) return;
    const generation = ++web.generation;
    window.clearTimeout(web.poll); web.poll = 0;
    web.applying = action; web.error = "";
    renderApps();
    const body = { action, revision: value.revision };
    if (action === "begin" || action === "clear" || action === "begin_creator") body.approve_profile = true;
    if (action === "confirm" || action === "cancel") body.attempt_id = value.attempt_id;
    if (action === "set_read") {
      body.allow_read = options.allowRead === true;
      if (options.allowRead === true && options.explicitReadScope === true) body.read_scope = options.readScope === "home_and_videos" ? "home_and_videos" : "home";
      if (typeof options.allowPlayback === "boolean") body.allow_playback = options.allowPlayback;
      if (typeof options.allowAudio === "boolean") body.allow_audio = options.allowAudio;
      if (typeof options.allowVisual === "boolean") body.allow_visual = options.allowVisual;
      if (typeof options.allowComments === "boolean") body.allow_comments = options.allowComments;
    }
    if (action === "set_publish") body.allow_publish = options.allowPublish === true;
    if (action === "set_interactions") {
      if (typeof options.allowLikes === "boolean") body.allow_likes = options.allowLikes;
      if (typeof options.allowReplies === "boolean") body.allow_replies = options.allowReplies;
    }
    let reloadAfterConflict = false;
    try {
      const updated = await request("/api/local/apps/douyin/web-login", { method: "POST", body: JSON.stringify(body) });
      if (current !== apps || web.generation !== generation) return;
      if (!validDouyinWebLogin(updated)) throw new Error("网页登录状态返回格式无效，请刷新后重试。");
      web.value = updated;
      web.loaded = true;
      let feedback = action === "clear" ? "已移除本机网页登录资料。" : action === "cancel" ? "已取消本次网页登录。" : action === "confirm" ? "网页登录资料已保存。" : action === "begin_creator" ? "已打开创作者中心登录窗口。" : "已打开网页登录窗口。";
      if (action === "set_read") {
        if (typeof options.allowComments === "boolean") feedback = options.allowComments ? "已允许读取视频评论。" : "已停止读取视频评论。";
        else if (typeof options.allowAudio === "boolean") feedback = options.allowAudio ? "已允许本机转写视频音轨。" : "已停止视频音轨转写。";
        else if (typeof options.allowVisual === "boolean") feedback = options.allowVisual ? "已允许使用本机视觉分析视频画面。" : "已停止视频画面分析。";
        else if (typeof options.allowPlayback === "boolean") feedback = options.allowPlayback ? "已允许静音观察视频播放。" : "已停止静音播放观察。";
        else feedback = options.allowRead ? (options.readScope === "home_and_videos" ? "已允许读取首页及视频页文字。" : "已允许读取抖音首页网页文字。") : "已停止网页读取。";
      }
      if (action === "set_publish") feedback = options.allowPublish ? "已允许普通账号发布。" : "已停止普通账号发布。";
      if (action === "set_interactions") {
        feedback = typeof options.allowReplies === "boolean"
          ? (options.allowReplies ? "已允许回复视频评论。" : "已停止回复视频评论。")
          : (options.allowLikes ? "已允许点赞。" : "已停止点赞。");
      }
      appFeedback(feedback);
    } catch (error) {
      if (current === apps && web.generation === generation && !appSessionExpired([{ status: "rejected", reason: error }])) {
        web.error = error.message || "网页登录操作未完成。";
        reloadAfterConflict = error.status === 409;
        // Inline failures stay beside their controls; conflict feedback remains
        // visible after the automatic reload replaces the inline state.
        appFeedback(reloadAfterConflict ? web.error : "", reloadAfterConflict);
      }
    } finally {
      if (current === apps && web.generation === generation) {
        web.applying = "";
        if (reloadAfterConflict) await loadDouyinWebLogin(current);
        else {
          if (shouldPollDouyinWebLogin(current, web.value)) scheduleDouyinWebLoginPoll(current);
          renderApps();
        }
      }
    }
  }
  function douyinWebLoginRow(current) {
    if (!current.registered?.some((item) => item.id === "douyin")) return null;
    const web = current.douyinWebLogin, value = web.value;
    const [title, note] = douyinWebLoginSummary(value);
    const row = appElement("section", "", "app-layer publication-layer douyin-web-login");
    const detail = appElement("div", "", "publication-detail");
    detail.append(appElement("strong", title), appElement("p", note));
    if (value?.confirmed_at) {
      const confirmed = new Date(value.confirmed_at);
      if (Number.isFinite(confirmed.getTime())) detail.append(appElement("p", `最近主人确认：${confirmed.toLocaleString()}`, "publication-reference"));
    }
    if (value?.reason) detail.append(appElement("p", value.reason, "publication-authorization-note"));
    if (web.error) detail.append(appElement("p", web.error, "publication-feedback is-warn"));
    const actions = appElement("div", "", "publication-actions douyin-web-login-actions");
    const busy = Boolean(web.loading || web.applying);
    const available = value?.available !== false;
    if (!web.loaded && !web.loading) {
      const retry = appElement("button", "读取网页登录状态", "button compact quiet");
      retry.type = "button"; retry.addEventListener("click", () => { void loadDouyinWebLogin(current); }); actions.append(retry);
    } else if (value) {
      if (!value.attempt_id || value.state === "saved") {
        const begin = appElement("button", web.applying === "begin" ? "正在打开…" : value.state === "saved" ? "重新打开登录窗口" : "打开网页登录窗口", "button compact secondary");
        begin.type = "button"; begin.disabled = busy || !available;
        begin.addEventListener("click", () => { void updateDouyinWebLogin(current, "begin", {}); }); actions.append(begin);
      }
      if (value.attempt_id) {
        const confirm = appElement("button", web.applying === "confirm" ? "正在保存…" : "我已完成登录，保存", "button compact primary");
        confirm.type = "button"; confirm.disabled = busy;
        confirm.addEventListener("click", () => { void updateDouyinWebLogin(current, "confirm", {}); });
        const cancel = appElement("button", web.applying === "cancel" ? "正在取消…" : "取消本次登录", "button compact quiet");
        cancel.type = "button"; cancel.disabled = busy;
        cancel.addEventListener("click", () => { void updateDouyinWebLogin(current, "cancel", {}); });
        actions.append(confirm, cancel);
      }
      if (["saved", "unconfirmed"].includes(value.state)) {
        const clear = appElement("button", web.applying === "clear" ? "正在移除…" : "移除网页登录资料", "button compact quiet");
        clear.type = "button"; clear.disabled = busy;
        clear.addEventListener("click", () => { void updateDouyinWebLogin(current, "clear", {}); }); actions.append(clear);
      }
      if (value.state === "saved" && !value.active && !value.attempt_id) {
        const publishEnabled = douyinWebPublishEnabled(value);
        const publish = document.createElement("input");
        publish.type = "checkbox";
        publish.checked = publishEnabled;
        publish.disabled = busy || (!publishEnabled && !available);
        publish.setAttribute("aria-label", "允许普通账号发布");
        publish.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_publish", { allowPublish: publish.checked }); });
        const publishLabel = appElement("label", "", "switch-row");
        publishLabel.append(publish, appElement("span", web.applying === "set_publish" ? "正在保存发布许可…" : "允许普通账号发布"));
        const creator = appElement("button", web.applying === "begin_creator" ? "正在打开…" : "登录创作者中心", "button compact secondary");
        creator.type = "button";
        creator.disabled = busy || !publishEnabled || !available;
        creator.addEventListener("click", () => { void updateDouyinWebLogin(current, "begin_creator", {}); });
        detail.append(
          appElement("p", publishEnabled ? "已保存普通账号发布许可。" : "普通账号发布许可未开启。", "publication-authorization-note"),
          appElement("p", "发布许可独立于网页文字读取。每次创作任务都要单独填写可见范围、作品内容和素材；发布回执未知时只核对原任务，不会重复发布。", "publication-authorization-note")
        );
        actions.append(publishLabel, creator);
        const readEnabled = douyinWebReadEnabled(value);
        const read = appElement("button", web.applying === "set_read" ? "正在保存…" : readEnabled ? "停止网页读取" : "允许读取首页文字", "button compact quiet");
        read.type = "button";
        read.disabled = busy || (!readEnabled && !available);
        read.addEventListener("click", () => { void updateDouyinWebLogin(current, "set_read", { allowRead: !readEnabled }); });
        const readNote = appElement("p", readEnabled ? "已允许读取实际网页文字。" : "可由主人单独允许读取首页网页文字。", "publication-authorization-note");
        readNote.append(readEnabled && douyinWebReadScope(value) === "home_and_videos"
          ? " 视频页文字不等于音轨或完整观看，也不授予点赞、评论或发布权限。"
          : " 不代表完整观看视频，也不授予点赞、评论或发布权限。");
        detail.append(readNote);
        actions.append(read);
        if (readEnabled) {
          const scope = document.createElement("select");
          scope.className = "douyin-web-read-scope";
          scope.setAttribute("aria-label", "读取范围");
          scope.disabled = busy;
          scope.append(new Option("仅首页文字", "home"), new Option("首页及视频页文字", "home_and_videos"));
          scope.value = douyinWebReadScope(value);
          scope.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_read", { allowRead: true, readScope: scope.value, explicitReadScope: true }); });
          const scopeLabel = appElement("label", "读取范围", "douyin-web-read-scope-label");
          scopeLabel.append(scope);
          actions.append(scopeLabel);
          if (douyinWebReadScope(value) === "home_and_videos") {
            const playbackLabel = appElement("label", "", "douyin-web-media-label douyin-web-playback-label");
            const playback = document.createElement("input");
            playback.type = "checkbox";
            playback.checked = douyinWebPlaybackEnabled(value);
            playback.disabled = busy || (!playback.checked && !available);
            playback.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_read", { allowRead: true, readScope: "home_and_videos", explicitReadScope: true, allowPlayback: playback.checked }); });
            playbackLabel.append(playback, document.createTextNode("允许静音观察视频播放"));
            actions.append(playbackLabel);
            const audioLabel = appElement("label", "", "douyin-web-media-label douyin-web-audio-label");
            const audio = document.createElement("input");
            audio.type = "checkbox";
            audio.checked = douyinWebAudioEnabled(value);
            audio.disabled = busy || (!audio.checked && !available);
            audio.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_read", { allowRead: true, readScope: "home_and_videos", explicitReadScope: true, allowAudio: audio.checked }); });
            audioLabel.append(audio, document.createTextNode("允许本机转写视频音轨"));
            actions.append(audioLabel);
            const visualLabel = appElement("label", "", "douyin-web-media-label douyin-web-visual-label");
            const visual = document.createElement("input");
            visual.type = "checkbox";
            visual.checked = douyinWebVisualEnabled(value);
            visual.disabled = busy || (!visual.checked && (!available || value.visual_ready !== true));
            visual.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_read", { allowRead: true, readScope: "home_and_videos", explicitReadScope: true, allowVisual: visual.checked }); });
            visualLabel.append(visual, document.createTextNode("允许本机分析视频画面"));
            actions.append(visualLabel);
            const commentsLabel = appElement("label", "", "douyin-web-media-label douyin-web-comments-label");
            const comments = document.createElement("input");
            comments.type = "checkbox";
            comments.checked = douyinWebCommentsEnabled(value);
            comments.disabled = busy || (!comments.checked && !available);
            comments.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_read", { allowRead: true, readScope: "home_and_videos", explicitReadScope: true, allowComments: comments.checked }); });
            commentsLabel.append(comments, document.createTextNode("允许读取视频评论"));
            actions.append(commentsLabel);
            const likesLabel = appElement("label", "", "douyin-web-media-label douyin-web-likes-label");
            const likes = document.createElement("input");
            likes.type = "checkbox";
            likes.checked = douyinWebLikesEnabled(value);
            likes.disabled = busy || (!likes.checked && !available);
            likes.setAttribute("aria-label", "允许点赞");
            likes.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_interactions", { allowLikes: likes.checked }); });
            likesLabel.append(likes, document.createTextNode(web.applying === "set_interactions" ? "正在保存互动许可…" : "允许点赞"));
            actions.append(likesLabel);
            const repliesLabel = appElement("label", "", "douyin-web-media-label douyin-web-replies-label");
            const replies = document.createElement("input");
            replies.type = "checkbox";
            replies.checked = douyinWebRepliesEnabled(value);
            replies.disabled = busy || (!replies.checked && (!available || !douyinWebCommentsEnabled(value)));
            replies.setAttribute("aria-label", "允许回复评论");
            replies.addEventListener("change", () => { void updateDouyinWebLogin(current, "set_interactions", { allowReplies: replies.checked }); });
            repliesLabel.append(replies, document.createTextNode(web.applying === "set_interactions" ? "正在保存互动许可…" : "允许回复评论"));
            actions.append(repliesLabel);
            const interactionNote = douyinWebCommentsEnabled(value)
              ? "点赞需要首页及视频页文字读取；回复评论还依赖视频评论读取。两项互动都单独保存。"
              : "点赞需要首页及视频页文字读取；回复评论还需先允许读取视频评论。";
            detail.append(appElement("p", value.visual_ready === true ? `播放、音轨、画面、评论和互动许可分别记录。${interactionNote} 画面分析仅将当前获准视频的实际采样帧交给媒体理解中配置的视觉服务处理。` : `${value.visual_reason || "请先在项目工具中配置媒体理解，画面分析许可暂不可开启。"} ${interactionNote}`, "publication-authorization-note"));
          }
        }
      }
    }
    if (actions.childElementCount) detail.append(actions);
    row.append(appElement("h5", "网页登录"), detail);
    return row;
  }
  function parseBilibiliVideos(value) {
    const unique = new Set();
    const invalid = [];
    for (const item of String(value || "").split(/[\s,，;；]+/).filter(Boolean)) {
      const match = /^bv([0-9a-z]{10})$/i.exec(item);
      if (!match) { invalid.push(item); continue; }
      unique.add("BV" + match[1]);
    }
    return { videos: Array.from(unique), invalid };
  }
  function bilibiliDraftFrom(settings) {
    return {
      revision: Number(settings?.revision || 0),
      enabled: Boolean(settings?.enabled),
      commentsEnabled: Boolean(settings?.comments_enabled),
      audioEnabled: Boolean(settings?.audio_enabled),
      browserEnabled: Boolean(settings?.browser_enabled),
      playbackEnabled: Boolean(settings?.playback_enabled),
      videos: Array.isArray(settings?.videos) ? settings.videos.join("\n") : "",
      dirty: false,
    };
  }
  function bilibiliPublicRow(record) {
    if (record.id !== "bilibili" || !apps.registered?.some((item) => item.id === "bilibili")) return null;
    const current = apps;
    const row = appElement("div", "", "app-layer");
    const detail = appElement("div", "");
    const settings = apps.bilibiliPublic;
    const draft = apps.bilibiliDraft || bilibiliDraftFrom(settings);
    if (!apps.bilibiliDraft) apps.bilibiliDraft = draft;
    const browserAvailable = settings?.browser_available === true;
    const audioAvailable = settings?.audio_available === true;
    const browserActive = Boolean(settings?.enabled && settings?.browser_enabled);
    const playbackAvailable = typeof settings?.playback_available === "boolean" ? settings.playback_available : browserAvailable;
    const playbackActive = Boolean(settings?.enabled && settings?.browser_enabled && settings?.playback_enabled);
    detail.append(
      appElement("strong", settings ? (settings.enabled ? `已授权 · ${settings.videos.length} 条稿件${settings.audio_enabled ? (audioAvailable ? " · 音轨已启用" : " · 音轨待配置") : ""}${settings.comments_enabled ? " · 评论已启用" : ""}${browserActive ? " · 独立浏览器已启用" : ""}${playbackActive ? " · 播放验证已启用" : ""}` : "读取未启用") : "读取设置暂不可用"),
      appElement("p", "匿名读取已列出的 B 站视频标题和投稿者简介。独立浏览器可读取获准页面的文字。")
    );
    if (apps.bilibiliError) detail.append(appElement("small", apps.bilibiliError));
    const form = appElement("form", "", "details-grid");
    const videos = document.createElement("textarea");
    videos.rows = 3;
    videos.placeholder = "每个 BV 号一行，也可用空格或逗号分隔";
    videos.value = draft.videos;
    videos.setAttribute("aria-label", "允许读取的视频 BV 号");
    videos.addEventListener("input", () => { draft.videos = videos.value; draft.dirty = true; });
    const enabled = document.createElement("input");
    enabled.type = "checkbox";
    enabled.checked = draft.enabled;
    const comments = document.createElement("input");
    comments.type = "checkbox";
    comments.checked = draft.commentsEnabled;
    const commentsLabel = appElement("label", "", "switch-row");
    commentsLabel.append(comments, appElement("span", "读取公开一级评论"));
    comments.addEventListener("change", () => { draft.commentsEnabled = comments.checked; draft.dirty = true; });
    const audio = document.createElement("input");
    audio.type = "checkbox";
    audio.checked = draft.audioEnabled;
    const audioLabel = appElement("label", "", "switch-row");
    audioLabel.append(audio, appElement("span", "使用本机语音识别读取音轨"));
    audio.addEventListener("change", () => { draft.audioEnabled = audio.checked; draft.dirty = true; });
    const audioNote = appElement("small", audioAvailable ? "按时间分段转写获准公开视频的音轨。" : "需准备 FFmpeg 和本机语音识别后端。");
    const browser = document.createElement("input");
    browser.type = "checkbox";
    browser.checked = draft.browserEnabled;
    browser.setAttribute("aria-label", "使用独立浏览器读取页面文本");
    const playback = document.createElement("input");
    playback.type = "checkbox";
    playback.checked = draft.playbackEnabled;
    playback.setAttribute("aria-label", "允许独立浏览器验证视频播放");
    const syncGrantToggles = () => {
      if (!draft.enabled) {
        draft.commentsEnabled = false;
        comments.checked = false;
        draft.audioEnabled = false;
        audio.checked = false;
        draft.browserEnabled = false;
        browser.checked = false;
      }
      if (!draft.browserEnabled) {
        draft.playbackEnabled = false;
        playback.checked = false;
      }
      comments.disabled = !draft.enabled;
      audio.disabled = !draft.enabled;
      browser.disabled = !draft.enabled || (!browserAvailable && !draft.browserEnabled);
      playback.disabled = !draft.enabled || !draft.browserEnabled || (!playbackAvailable && !draft.playbackEnabled);
    };
    syncGrantToggles();
    enabled.addEventListener("change", () => {
      draft.enabled = enabled.checked;
      draft.dirty = true;
      syncGrantToggles();
    });
    const enabledLabel = appElement("label", "", "switch-row");
    enabledLabel.append(enabled, appElement("span", "允许大肥鱼按需读取这些公开资料"));
    browser.addEventListener("change", () => { draft.browserEnabled = browser.checked; draft.dirty = true; syncGrantToggles(); });
    const browserLabel = appElement("label", "", "switch-row");
    browserLabel.append(browser, appElement("span", "使用独立浏览器读取页面文本"));
    const browserNote = appElement("small", browserAvailable ? "读取获准 BV 页面时启动无头 Edge，数据保存在大肥鱼的专属目录。" : (draft.browserEnabled ? "当前 Windows 未找到可用的 Edge，可以保存并关闭独立浏览器。" : "需要安装 Microsoft Edge 才能开启。"));
    playback.addEventListener("change", () => { draft.playbackEnabled = playback.checked; draft.dirty = true; });
    const playbackLabel = appElement("label", "", "switch-row");
    playbackLabel.append(playback, appElement("span", "允许独立浏览器验证视频播放"));
    const playbackNote = appElement("small", playbackAvailable ? "静音采样获准 BV 的公开媒体，按实际播放区间显示覆盖范围。" : (draft.playbackEnabled ? "当前 Windows 未找到可用的 Edge，可以保存并关闭播放验证。" : "需要先开启独立浏览器并安装 Microsoft Edge。"));
    const actions = appElement("div", "", "app-worker-actions");
    const save = appElement("button", apps.bilibiliSaving ? "正在保存…" : "保存公开读取设置", "button compact secondary");
    save.type = "submit";
    save.disabled = !settings || apps.loading || apps.bilibiliSaving;
    actions.append(save, appElement("small", "实际读取时才会向 B 站请求对应稿件。"));
    form.append(appElement("label", "允许读取的视频 BV 号"), videos, enabledLabel, commentsLabel, audioLabel, audioNote, browserLabel, browserNote, playbackLabel, playbackNote, actions);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!settings || current !== apps || current.bilibiliSaving) return;
      const parsed = parseBilibiliVideos(draft.videos);
      if (parsed.invalid.length || parsed.videos.length > 100 || (draft.enabled && !parsed.videos.length) || (draft.enabled && draft.browserEnabled && !browserAvailable) || (draft.enabled && draft.playbackEnabled && (!draft.browserEnabled || !playbackAvailable))) {
        const reason = parsed.invalid.length ? `BV 号格式不正确：${parsed.invalid.slice(0, 3).join("、")}` : parsed.videos.length > 100 ? "最多可保存 100 个 BV 号。" : draft.enabled && draft.browserEnabled && !browserAvailable ? "当前 Windows 未找到可用的 Edge，不能启用独立浏览器。" : draft.enabled && draft.playbackEnabled && !draft.browserEnabled ? "请先开启独立浏览器。" : draft.enabled && draft.playbackEnabled && !playbackAvailable ? "当前 Windows 未找到可用的 Edge，不能启用播放验证。" : "启用前请至少填写一个 BV 号。";
        appFeedback(reason, true);
        return;
      }
      apps.bilibiliSaving = true;
      apps.bilibiliError = "";
      renderApps();
      try {
        const value = await request("/api/local/apps/bilibili/public", { method: "PUT", body: JSON.stringify({ revision: Number(draft.revision || 0), enabled: Boolean(draft.enabled), comments_enabled: Boolean(draft.enabled && draft.commentsEnabled), audio_enabled: Boolean(draft.enabled && draft.audioEnabled), browser_enabled: Boolean(draft.enabled && draft.browserEnabled), playback_enabled: Boolean(draft.enabled && draft.browserEnabled && draft.playbackEnabled), videos: parsed.videos }) });
        if (current !== apps) return;
        if (!value || !Number.isFinite(Number(value.revision)) || !Array.isArray(value.videos) || typeof value.browser_enabled !== "boolean" || typeof value.browser_available !== "boolean" || typeof value.playback_enabled !== "boolean" || typeof value.comments_enabled !== "boolean" || typeof value.audio_enabled !== "boolean" || typeof value.audio_available !== "boolean") throw new Error("公开读取设置返回格式无效，请刷新后重试。");
        apps.bilibiliPublic = value;
        apps.bilibiliDraft = { revision: Number(value.revision), enabled: Boolean(value.enabled), browserEnabled: Boolean(value.browser_enabled), playbackEnabled: Boolean(value.playback_enabled), videos: value.videos.join("\n"), dirty: false };
        appFeedback(value.enabled ? "公开读取设置已保存。" : "公开读取已关闭，已保留 BV 号列表。");
      } catch (error) {
        if (current !== apps) return;
        if (appSessionExpired([{ status: "rejected", reason: error }])) return;
        if (error.status === 409) {
          const saved = await request("/api/local/apps/bilibili/public").catch(() => null);
          if (current !== apps) return;
          if (saved) {
            apps.bilibiliPublic = saved;
            draft.revision = saved.revision;
          }
          apps.bilibiliError = saved ? `设置已更新为${saved.enabled ? "启用" : "停用"}，已保存 BV 号 ${saved.videos.join("、") || "无"}。你的输入仍在表单中。` : "设置已变化，请刷新后重新核对。";
        } else {
          apps.bilibiliError = error.message;
        }
        appFeedback(apps.bilibiliError, true);
      } finally {
        current.bilibiliSaving = false;
        if (current === apps) renderApps();
      }
    });
    detail.append(form);
    row.append(appElement("h5", "公开读取"), detail);
    return row;
  }
  function renderApps() {
    const list = $("#apps-list");
    list.setAttribute("aria-busy", String(apps.loading || Boolean(apps.installing)));
    $("#apps-refresh").disabled = apps.loading || Boolean(apps.installing);
    $("#apps-refresh").textContent = apps.loading ? "刷新中…" : "刷新";
    const catalog = new Map((apps.catalog || []).map((row) => [row.id, row]));
    const records = new Map((apps.registered || []).map((row) => [row.id, row]));
    for (const row of catalog.values()) records.set(row.id, row);
    if (!records.size) {
      const text = apps.loading ? "正在读取应用…" : apps.catalog && apps.registered ? "还没有可管理的应用。" : "暂时无法读取应用，可刷新后重查。";
      list.replaceChildren(appElement("p", text, "apps-empty"));
      return;
    }
    list.replaceChildren(...Array.from(records.values(), (record) => {
      const card = appElement("article", "", "app-record");
      const head = appElement("header", "", "app-record-head");
      const label = record.name || record.id;
      const mark = appElement("span", Array.from(label)[0], "app-mark");
      mark.setAttribute("aria-hidden", "true");
      head.append(mark, appElement("h4", label));
      const open = appElement("button", "", "app-card-entry");
      open.type = "button"; open.setAttribute("aria-label", `管理${label}`);
      const accounts = apps.connections.get(record.id) || [];
      const authorization = record.id === "bilibili" ? apps.bilibiliAuthorization : null;
      const officialAccount = record.id === "bilibili";
      const douyinLogin = record.id === "douyin" ? apps.douyinWebLogin?.value : null;
      const authenticated = accounts.filter((account) => apps.accountStatuses.get(account.id)?.login === "authenticated").length;
      const connected = record.id === "douyin" ? douyinLogin?.state === "saved" : officialAccount ? authorization?.status === "ready" : authenticated > 0;
      const status = appElement("span", connected ? "已接入" : record.id === "douyin" && douyinLogin?.state === "unconfirmed" ? "等待确认" : authorization?.status === "expired" ? "登录已过期" : !officialAccount && accounts.length ? "账号已登记" : "待接入", "app-card-status");
      status.classList.toggle("is-connected", connected);
      head.append(status);
      const summary = appElement("p", record.id === "douyin" && connected ? "网页登录资料已保存，可在授权范围内使用抖音功能。" : officialAccount && connected ? (authorization.label || "账号已授权，可管理使用范围。") : !officialAccount && accounts.length ? `${accounts.length} 个账号 · ${authenticated} 个已登录` : "连接账号后，大肥鱼就能在这里活动。", "app-card-summary");
      const source = catalog.get(record.id);
      const foot = appElement("div", "", "app-card-footer");
      foot.append(appElement("span", source?.install_state === "installed" ? "接入版本已安装" : source?.source_available ? "有可用版本" : "暂无可用版本"), appElement("span", "管理应用"));
      open.append(head, summary, foot); card.append(open);
      open.addEventListener("click", () => {
        apps.openID = record.id; apps.detailTab = "account"; renderAppDialog(record, source); $("#app-dialog").showModal();
        if (record.id === "douyin") {
          void loadDouyinWebLogin(apps);
          if (apps.douyinActivityTab === "publications") void loadPublicationWork(apps, "douyin", apps.douyinWork.cursors);
        }
      });
      return card;
    }));
    if ($("#app-dialog").open && records.has(apps.openID)) renderAppDialog(records.get(apps.openID), catalog.get(apps.openID));
  }
  function renderAppDialog(record, listing) {
    const body = $("#app-dialog-body"), scroll = body.scrollTop;
    $("#app-dialog-feedback").append($("#apps-feedback"));
    $("#app-dialog-title").textContent = `${record.name || record.id} · 应用设置`;
    const nav = $("#app-dialog-tabs");
    nav.replaceChildren(...[["account", "账号与使用"], ["persona", "应用子角色"], ["source", "接入版本管理"]].map(([id, title]) => {
      const button = appElement("button", title, `local-tab${apps.detailTab === id ? " is-active" : ""}`);
      button.type = "button"; button.setAttribute("aria-pressed", String(apps.detailTab === id));
      button.addEventListener("click", () => {
        apps.detailTab = id;
        if (record.id === "douyin" && id !== "account") {
          stopDouyinWebLoginPolling(apps);
          stopDouyinInteractionWork(apps);
          stopDouyinLiveInspection(apps);
        }
        if (record.id === "douyin" && id === "account") void loadDouyinWebLogin(apps);
        renderAppDialog(record, listing); body.scrollTop = 0;
      }); return button;
    }));
    const controls = apps.detailTab === "source" ? [appSourceRow(record, listing)] : apps.detailTab === "persona" ? [appPersonaRow(record)] :
      record.id === "douyin" ? [douyinWebLoginRow(apps), douyinLiveClientRow(apps), douyinPublicationRow(apps)] :
      record.id === "bilibili" ? [bilibiliPublicRow(record), bilibiliPublicationRow(apps)] : [appConnectionRow(record)];
    body.replaceChildren(...controls.filter(Boolean)); body.scrollTop = scroll;
    $("#app-dialog-refresh").disabled = apps.loading;
    $("#app-dialog-refresh").textContent = apps.loading ? "刷新中…" : "刷新";
  }
  function stopDouyinLiveInspection(current) {
    const draft = current.douyinLiveClient;
    draft.generation++; draft.controller?.abort(); draft.controller = null; draft.busy = false;
  }
  function douyinLiveClientRow(current) {
    const draft = current.douyinLiveClient;
    const form = appElement("form", "", "app-layer publication-layer douyin-live-client");
    form.setAttribute("aria-label", "直播伴侣程序检查");
    const detail = appElement("div", "", "publication-detail");
    detail.append(appElement("strong", "直播伴侣"), appElement("p", "安装后选择主程序检查版本；不会打开程序或开始直播。"));
    const field = appElement("label", "已安装程序的 EXE 路径", "settings-field");
    const input = document.createElement("input");
    input.type = "text"; input.value = draft.path; input.maxLength = 4096; input.autocomplete = "off";
    input.placeholder = "填写已安装主程序的完整路径"; input.disabled = draft.busy;
    input.setAttribute("aria-label", "直播伴侣主程序路径"); field.append(input);
    const result = appElement("div", "", "publication-feedback"); result.setAttribute("aria-live", "polite");
    if (draft.error) { result.classList.add("is-warn"); result.textContent = draft.error; }
    else if (draft.result) {
      const file = draft.result.file || {};
      const identity = [file.product_name, file.version, file.architecture].filter(value => typeof value === "string" && value).join(" · ");
      if (identity) result.append(appElement("p", identity));
      result.append(appElement("p", draft.result.reason || "已检查文件，直播控制尚未验证。"));
    }
    const actions = appElement("div", "", "publication-actions");
    const inspect = appElement("button", draft.busy ? "检查中…" : "检查程序", "button compact quiet");
    inspect.type = "submit"; inspect.disabled = draft.busy || !draft.path.trim(); actions.append(inspect);
    input.addEventListener("input", () => {
      draft.path = input.value; draft.result = null; draft.error = ""; result.replaceChildren();
      inspect.disabled = !draft.path.trim();
    });
    form.addEventListener("submit", async event => {
      event.preventDefault(); if (draft.busy || !draft.path.trim()) return;
      const generation = ++draft.generation;
      const controller = new AbortController(); draft.controller = controller; draft.busy = true; draft.error = ""; draft.result = null;
      renderApps();
      const selected = () => current === apps && generation === draft.generation && activeView === "apps" && $("#app-dialog").open && current.openID === "douyin" && current.detailTab === "account";
      try {
        const value = await request("/api/local/apps/douyin/live-client/inspect", { method: "POST", body: JSON.stringify({ executable: draft.path }), signal: controller.signal });
        if (!selected()) return;
        if (!["inspected", "metadata_missing"].includes(value.state) || value.control_available !== false || !value.file || typeof value.reason !== "string") throw new Error("程序检查返回不完整，请重新检查。");
        draft.result = value;
      } catch (error) {
        if (!selected()) return;
        if (error.status === 401) { appSessionExpired([{ status: "rejected", reason: error }]); return; }
        draft.error = error.message;
      } finally {
        if (generation === draft.generation) { draft.busy = false; draft.controller = null; if (selected()) renderApps(); }
      }
    });
    detail.append(field, result); form.append(detail, actions); return form;
  }
  function appPersonaRow(record) {
    const current = apps, id = record.id, label = record.name || id;
    const draft = current.personas.get(id) || { revision: 0, text: "", loaded: false, editing: false, busy: false, error: "" };
    current.personas.set(id, draft);
    const form = appElement("form", "", "app-persona-form");
    form.setAttribute("aria-label", `${label}子角色`);
    const heading = appElement("div", "", "app-persona-heading");
    const edit = appElement("button", draft.busy ? "读取中…" : "编辑", "text-action");
    edit.type = "button"; edit.disabled = draft.busy; edit.hidden = draft.editing;
    heading.append(appElement("strong", "应用子角色"), edit);
    const note = appElement("p", draft.error || (draft.editing ? "仅在本应用使用，留空沿用主角色。" : draft.loaded && draft.text ? draft.text : "在主角色之上，补充本应用的表达方式、兴趣和偏好。"), "app-persona-note");
    const input = document.createElement("textarea");
    input.setAttribute("aria-label", `${label}子角色设定`); input.maxLength = 4000; input.rows = 3;
    input.placeholder = "留空沿用主角色。例如：在这个应用里更喜欢聊音乐…"; input.value = draft.text; input.hidden = !draft.editing;
    input.addEventListener("input", () => { draft.text = input.value; });
    const actions = appElement("div", "", "settings-actions"); actions.hidden = !draft.editing;
    const save = appElement("button", draft.busy ? "保存中…" : "保存子角色", "button compact primary"); save.type = "submit"; save.disabled = draft.busy;
    const cancel = appElement("button", "取消", "text-action"); cancel.type = "button"; cancel.disabled = draft.busy;
    actions.append(save, cancel);
    edit.addEventListener("click", async () => {
      draft.busy = true; edit.disabled = true;
      try {
        if (!draft.loaded) { const value = await request(`/api/local/apps/${encodeURIComponent(id)}/persona`); if (current !== apps) return; draft.revision = value.revision || 0; draft.text = value.persona || ""; draft.loaded = true; }
        draft.editing = true; draft.error = "";
      } catch (error) { draft.error = error.message; }
      finally { draft.busy = false; if (current === apps) renderApps(); }
    });
    cancel.addEventListener("click", () => { draft.editing = false; draft.loaded = false; draft.error = ""; renderApps(); });
    form.addEventListener("submit", async (event) => {
      event.preventDefault(); if (draft.busy || !draft.editing) return;
      draft.busy = true; save.disabled = true;
      try {
        const value = await request(`/api/local/apps/${encodeURIComponent(id)}/persona`, { method: "PUT", body: JSON.stringify({ revision: draft.revision, persona: draft.text }) });
        if (current !== apps) return;
        draft.revision = value.revision; draft.text = value.persona; draft.editing = false; draft.error = "";
        appFeedback(`${label}子角色已保存，下次模型调用生效。`);
      } catch (error) {
        draft.error = error.message;
        if (error.status === 409) { try { const latest = await request(`/api/local/apps/${encodeURIComponent(id)}/persona`); if (current === apps) draft.revision = latest.revision; } catch (_) { /* Keep the draft and its revision until a fresh read succeeds. */ } }
      } finally { draft.busy = false; if (current === apps) renderApps(); }
    });
    form.append(heading, note, input, actions); return form;
  }
  async function loadApps() {
    const current = apps;
    if (current.loading || !state.home?.initialized) return;
    current.loading = true;
    renderApps();
    try {
      const result = await Promise.allSettled([request("/api/local/apps/catalog"), request("/api/apps")]);
      if (current !== apps || appSessionExpired(result)) return;
      const registered = appRows(result[1], "items");
      const connections = await Promise.allSettled((registered || []).map((row) => request(`/api/apps/${encodeURIComponent(row.id)}/connections`)));
      if (current !== apps || appSessionExpired(connections)) return;
      const connectionMap = new Map((registered || []).map((row, index) => [row.id, appRows(connections[index], "items")]));
      const nativeAccounts = connectionMap.get("wechat") && registered.some((app) => app.id === "wechat" && app.registration === "station-native")
        ? connectionMap.get("wechat") : [];
      const statuses = await Promise.allSettled(nativeAccounts.map((account) => request(`/api/apps/wechat/connections/${encodeURIComponent(account.id)}`)));
      if (current !== apps || appSessionExpired(statuses)) return;
      const hasBilibili = registered?.some((app) => app.id === "bilibili");
      let bilibili = null;
      let authorization = null;
      // Douyin activity is loaded only when the owner opens its application
      // detail and selects a record tab. A catalogue refresh never reads it.
      if (hasBilibili) {
        const result = await Promise.allSettled([
          request("/api/local/apps/bilibili/public"),
          request("/api/local/apps/bilibili/publication-authorization"),
          loadBilibiliWork(current, current.bilibiliWork.cursors)
        ]);
        if (current !== apps || appSessionExpired(result)) return;
        bilibili = result[0];
        authorization = result[1];
      }
      current.catalog = appRows(result[0], "applications");
      current.registered = registered;
      current.connections = connectionMap;
      current.accountStatuses = new Map(nativeAccounts.map((account, index) => {
        const entry = statuses[index];
        const details = entry.status === "fulfilled" ? entry.value.connection : null;
        return [account.id, details?.id === account.id ? details : null];
      }));
      if (bilibili?.status === "fulfilled") {
        const value = bilibili.value;
        current.bilibiliPublic = value;
        current.bilibiliError = "";
        if (!current.bilibiliDraft || !current.bilibiliDraft.dirty) current.bilibiliDraft = bilibiliDraftFrom(value);
      } else if (hasBilibili) {
        current.bilibiliPublic = null;
        current.bilibiliError = bilibili?.reason?.message || "公开读取设置暂不可读。";
      } else {
        current.bilibiliPublic = null;
        current.bilibiliDraft = null;
        current.bilibiliError = "";
      }
      if (authorization?.status === "fulfilled") {
        const value = authorization.value;
        current.bilibiliAuthorization = validPublicationAuthorization(value) ? value : null;
        current.bilibiliAuthorizationError = current.bilibiliAuthorization ? "" : "发布授权状态返回格式无效，请刷新后重试。";
      } else if (hasBilibili) {
        current.bilibiliAuthorization = null;
        current.bilibiliAuthorizationError = authorization?.reason?.message || "发布授权状态暂不可读。";
      } else {
        current.bilibiliAuthorization = null;
        current.bilibiliAuthorizationError = "";
      }
    } finally {
      if (current === apps) {
        current.loading = false;
        renderApps();
      }
    }
  }

  const publicationStates = {
    draft: ["草稿", "成品已登记，等待提交。"],
    accepted: ["已受理", "平台已返回作品回执，可继续查询是否公开。"],
    published: ["已公开", "平台已确认作品可公开浏览。"],
    cancelled: ["已停止", "此任务的后续操作已停止。"],
    failed: ["未完成", "本次发布未完成。"],
    awaiting_receipt: ["待核对", "查询原任务以查看最新状态。"],
  };
  const publicationStages = {
	opening_creator: ["正在打开创作者中心", "正在准备账号页面。"],
	selecting_file: ["正在选择成品", "正在将已核对的成品交给上传页面。"],
	editing: ["正在填写作品信息", "正在填写标题、简介和可见范围。"],
	verifying_form: ["正在核对发布设置", "等待视频预览和指定可见范围准备完成。"],
    initializing: ["等待上传凭证", "等待确认原上传是否已初始化。"],
    uploading: ["分片上传", "已确认的分片会保留。"],
    merging: ["等待合并回执", "全部分片已确认，合并结果待核对。"],
    merged: ["上传已合并", "可从原上传继续投稿。"],
    posting: ["投稿结果待核对", "平台尚未返回原投稿的稿件编号。"],
    part_pending: ["分片结果待核对", "有一个分片没有回执，后续上传已暂停。"],
  };
  const publicationActions = { submit: "提交投稿", resume: "继续任务", status: "查询状态", cancel: "停止后续" };
  function publicationStatus(task) {
    if (task.state === "failed" && task.progress?.stage === "rejected") return ["未提交成功", "本次发布已被拒绝。"];
    const normal = publicationStates[task.state] || publicationStates.awaiting_receipt;
	const creatorIssues = {
	  creator_login_required: "创作者中心需要重新登录，请在抖音应用详情打开登录窗口。",
	  creator_profile_busy: "专属登录窗口仍在使用，关闭并确认保存后可继续原任务。",
	  creator_controls_unavailable: "创作者页面未准备好，或网页控件发生变化；尚未确认提交。",
	  creator_upload_incomplete: "视频上传尚未完成，或页面已清除所选文件。",
	  creator_visibility_unconfirmed: "网页未确认作品可见范围，未继续发布。",
	  creator_caption_unconfirmed: "网页未确认填写的标题或简介，未继续发布。",
	  creator_result_unknown: "提交结果尚未确认，只查询原任务，避免重复发布。",
	  creator_browser_unavailable: "专属浏览器暂不可用，原任务已保留。",
	  creator_timeout: "创作者页面处理超时，原任务和已完成进度保留。",
	  creator_cancelled: "本次操作已中断，请先核对原任务。",
	  creator_failed: "创作者页面处理失败，原任务已保留。",
	};
	if (task.state === "awaiting_receipt" && creatorIssues[task.progress?.error_code]) return ["等待继续", creatorIssues[task.progress.error_code]];
    if (task.state !== "awaiting_receipt") return normal;
    if (task.cancel_requested) return ["已要求停止", "已请求停止后续操作，原投稿结果仍待核对。"];
    return publicationStages[task.progress?.stage] || normal;
  }
  function publicationAuthorizationSummary(authorization) {
    if (authorization?.refresh_pending) return ["续期结果未确认", "重新网页授权后可恢复投稿。"];
    const states = {
      unconfigured: ["尚未登记发布授权", "登记开放平台凭据后，宿主会向 B 站核对账号。"],
      legacy: ["旧版发布授权待升级", "请先用现有凭据核验账号，再更换令牌。"],
      ready: ["发布授权已登记", "账号已核验，可更新同一应用和账号的令牌。"],
      disabled: ["发布授权已停用", "仍可查看已有任务。重新启用时会核验账号。"],
      expired: ["发布授权已过期", "请登记同一账号的有效凭据。"],
    };
    return states[authorization?.state] || ["发布授权暂不可读", "刷新应用状态后重查。"];
  }
  function validPublicationAuthorization(value) {
    return value && typeof value.revision === "string" && ["unconfigured", "legacy", "ready", "disabled", "expired"].includes(value.state) &&
      typeof value.verified === "boolean" && typeof value.enabled === "boolean";
  }
  function bilibiliAuthorizeURL(value) {
    if (typeof value !== "string") return "";
    try {
      const url = new URL(value);
      return url.origin === "https://account.bilibili.com" && !url.username && !url.password && !url.hash &&
        url.pathname === "/pc/account-pc/auth/oauth" ? url.href : "";
    } catch (_) { return ""; }
  }
  function publicationOAuthNote(authorization) {
    if (authorization?.refresh_available) return "当前授权有效，可按需手动续期。";
    return "填写开放平台已登记的回调地址，并在 B 站勾选账号查询和视频投稿权限。";
  }
  function bilibiliOAuthForm(current) {
    const authorization = current.bilibiliAuthorization;
    const flow = current.bilibiliOAuth;
    const panel = appElement("details", "", "publication-oauth");
    panel.append(appElement("summary", "网页授权"));
    const form = appElement("form", "", "publication-oauth-form");
    const note = appElement("p", publicationOAuthNote(authorization), "publication-authorization-note");
    const clientID = document.createElement("input");
    clientID.name = "client_id"; clientID.autocomplete = "off"; clientID.required = true; clientID.placeholder = "B 站 Client ID"; clientID.setAttribute("aria-label", "OAuth Client ID");
    const appSecret = document.createElement("input");
    appSecret.name = "app_secret"; appSecret.type = "password"; appSecret.autocomplete = "new-password"; appSecret.required = true; appSecret.placeholder = "App Secret"; appSecret.setAttribute("aria-label", "OAuth App Secret");
    const redirectURI = document.createElement("input");
    redirectURI.name = "redirect_uri"; redirectURI.type = "url"; redirectURI.autocomplete = "off"; redirectURI.required = true; redirectURI.placeholder = "https://…/callback"; redirectURI.setAttribute("aria-label", "回调地址");
    const enabled = document.createElement("input");
    enabled.type = "checkbox"; enabled.name = "enabled"; enabled.checked = Boolean(authorization?.enabled);
    const enabledLabel = appElement("label", "", "switch-row");
    enabledLabel.append(enabled, appElement("span", "完成授权后启用发布授权"));
    const begin = appElement("button", flow.saving ? "正在生成…" : "生成授权链接", "button compact secondary");
    begin.type = "submit"; begin.disabled = !authorization || flow.saving;
    const actions = appElement("div", "", "app-worker-actions"); actions.append(begin);
    for (const [field, label] of [[clientID, "Client ID"], [appSecret, "App Secret"], [redirectURI, "回调地址"]]) {
      field.id = `bilibili-oauth-${field.name}`;
      const labelNode = appElement("label", label); labelNode.htmlFor = field.id; form.append(labelNode, field);
    }
    form.append(note, enabledLabel, actions);
    const callback = document.createElement("input");
    callback.type = "url"; callback.autocomplete = "off"; callback.required = true; callback.placeholder = "粘贴浏览器返回的完整回调地址"; callback.setAttribute("aria-label", "完整回调地址");
    const complete = appElement("button", "完成网页授权", "button compact primary"); complete.type = "button";
    const callbackActions = appElement("div", "", "app-worker-actions"); callbackActions.append(complete);
    const callbackBlock = appElement("div", "", "publication-oauth-callback");
    const link = appElement("a", "在浏览器中打开 B 站授权", "button compact quiet");
    link.target = "_blank"; link.rel = "noopener noreferrer";
    const copy = appElement("button", "复制授权链接", "button compact quiet"); copy.type = "button";
    copy.addEventListener("click", async () => {
      try { await navigator.clipboard.writeText(flow.authorizeURL); appFeedback("授权链接已复制，可粘贴到浏览器打开。"); }
      catch (_) { appFeedback("无法自动复制，请使用上方链接或手动复制。", true); }
    });
    const expiration = appElement("p", "", "publication-reference");
    const showCallback = () => {
      callbackBlock.hidden = !flow.authorizeURL;
      callback.disabled = callbackBlock.hidden;
      if (!flow.authorizeURL) return;
      link.href = flow.authorizeURL;
      expiration.textContent = `授权链接有效至：${new Date(flow.expiresAt).toLocaleString()}`;
    };
    callbackBlock.append(appElement("p", "在浏览器完成授权后，把地址栏中的完整回跳地址粘贴到这里。"), expiration, link, copy, callback, callbackActions);
    showCallback();
    form.append(callbackBlock);
    panel.append(form);
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!authorization || current !== apps || flow.saving) return;
      const credentials = { client_id: clientID.value, app_secret: appSecret.value, redirect_uri: redirectURI.value, enabled: enabled.checked };
      clientID.value = appSecret.value = redirectURI.value = "";
      flow.authorizeURL = flow.expiresAt = callback.value = "";
      showCallback();
      flow.saving = true; begin.disabled = true;
      try {
        const value = await request("/api/local/apps/bilibili/publication-oauth", { method: "POST", body: JSON.stringify({ action: "begin", revision: authorization.revision, ...credentials }) });
        if (current !== apps) return;
        const authorizeURL = bilibiliAuthorizeURL(value.authorize_url);
        if (!authorizeURL || !Number.isFinite(Date.parse(value.expires_at))) throw new Error("授权链接返回格式无效，请重新生成。");
        flow.authorizeURL = authorizeURL; flow.expiresAt = value.expires_at;
        showCallback();
        callback.focus();
      } catch (error) { if (current === apps && !appSessionExpired([{ status: "rejected", reason: error }])) { current.bilibiliAuthorizationError = error.message; appFeedback(error.message, true); } }
      finally { flow.saving = false; begin.disabled = false; }
    });
    complete.addEventListener("click", async () => {
      if (!flow.authorizeURL || current !== apps || flow.saving || !callback.reportValidity()) return;
      const callbackURL = callback.value; callback.value = ""; flow.saving = true; complete.disabled = true;
      try {
        const value = await request("/api/local/apps/bilibili/publication-oauth", { method: "POST", body: JSON.stringify({ action: "complete", callback_url: callbackURL }) });
        if (current !== apps) return;
        if (!validPublicationAuthorization(value)) throw new Error("发布授权状态返回格式无效，请刷新后重试。");
        current.bilibiliAuthorization = value; flow.authorizeURL = flow.expiresAt = ""; flow.saving = false;
        appFeedback(value.enabled ? "网页授权已完成并启用。" : "网页授权已完成，但目前未启用。"); await loadApps();
      } catch (error) { if (current === apps && !appSessionExpired([{ status: "rejected", reason: error }])) { current.bilibiliAuthorizationError = error.message; appFeedback(error.message, true); } }
      finally { callback.value = ""; flow.saving = false; complete.disabled = false; }
    });
    callback.addEventListener("keydown", (event) => {
      if (event.key === "Enter") { event.preventDefault(); complete.click(); }
    });
    return panel;
  }
  function publicationRefreshButton(current, authorization) {
    if (!authorization?.refresh_available || authorization.refresh_pending) return null;
    const button = appElement("button", current.bilibiliAuthorizationSaving ? "正在续期…" : "手动续期", "button compact quiet"); button.type = "button"; button.disabled = current.loading || current.bilibiliAuthorizationSaving;
    button.addEventListener("click", async () => {
      if (current !== apps || current.loading || current.bilibiliAuthorizationSaving) return;
      current.bilibiliAuthorizationSaving = true; updatePublicationRow(current);
      try {
        const value = await request("/api/local/apps/bilibili/publication-oauth", { method: "POST", body: JSON.stringify({ action: "refresh", revision: authorization.revision }) });
        if (!validPublicationAuthorization(value)) throw new Error("发布授权状态返回格式无效，请刷新后重试。");
        current.bilibiliAuthorization = value; appFeedback(value.refresh_pending ? "续期不可用，需要重新网页授权。" : "发布授权已手动续期。"); await loadApps();
      } catch (error) {
        if (current === apps && !appSessionExpired([{ status: "rejected", reason: error }])) {
          appFeedback(error.message, true);
          await loadApps();
        }
      }
      finally { current.bilibiliAuthorizationSaving = false; if (current === apps) updatePublicationRow(current); }
    });
    return button;
  }
  function bilibiliAuthorizationForm(current) {
    const authorization = current.bilibiliAuthorization;
    const panel = appElement("details", "", "publication-authorization");
    panel.open = false;
    const summary = appElement("summary", "登记发布授权");
    panel.append(summary);
    const form = appElement("form", "", "publication-authorization-form");
    const intro = appElement("p", "账号核验需要 USER_INFO 授权。视频投稿另需 B 站批准 ARC_BASE 权限。", "publication-authorization-note");
    const clientID = document.createElement("input");
    clientID.name = "client_id";
    clientID.autocomplete = "off";
    clientID.required = true;
    clientID.placeholder = "B 站 Client ID";
    clientID.setAttribute("aria-label", "Client ID");
    const appSecret = document.createElement("input");
    appSecret.name = "app_secret";
    appSecret.type = "password";
    appSecret.autocomplete = "new-password";
    appSecret.required = true;
    appSecret.placeholder = "App Secret";
    appSecret.setAttribute("aria-label", "App Secret");
    const accessToken = document.createElement("input");
    accessToken.name = "access_token";
    accessToken.type = "password";
    accessToken.autocomplete = "new-password";
    accessToken.required = true;
    accessToken.placeholder = "Access Token";
    accessToken.setAttribute("aria-label", "Access Token");
    const expiresAt = document.createElement("input");
    expiresAt.name = "expires_at";
    expiresAt.type = "datetime-local";
    expiresAt.required = true;
    expiresAt.setAttribute("aria-label", "凭据到期时间");
    const enabled = document.createElement("input");
    enabled.type = "checkbox";
    enabled.name = "enabled";
    enabled.checked = Boolean(authorization?.enabled);
    const enabledLabel = appElement("label", "", "switch-row");
    enabledLabel.append(enabled, appElement("span", "完成核验后启用发布授权"));
    const actions = appElement("div", "", "app-worker-actions");
    const verify = appElement("button", current.bilibiliAuthorizationSaving ? "正在核验…" : "核验并登记", "button compact secondary");
    verify.type = "submit";
    verify.disabled = !authorization || current.bilibiliAuthorizationSaving;
    const reset = appElement("button", "清空输入", "button compact quiet");
    reset.type = "reset";
    reset.disabled = current.bilibiliAuthorizationSaving;
    actions.append(verify, reset);
    const fields = [[clientID, "Client ID"], [appSecret, "App Secret"], [accessToken, "Access Token"], [expiresAt, "凭据到期时间（本地时间）"]];
    form.append(intro);
    for (const [field, label] of fields) {
      field.id = `bilibili-publication-${field.name}`;
      const labelNode = appElement("label", label);
      labelNode.htmlFor = field.id;
      form.append(labelNode, field);
    }
    form.append(enabledLabel, actions);
    form.addEventListener("reset", () => { window.setTimeout(() => { clientID.value = appSecret.value = accessToken.value = expiresAt.value = ""; }, 0); });
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!authorization || current !== apps || current.bilibiliAuthorizationSaving) return;
      const expires = new Date(expiresAt.value);
      if (!Number.isFinite(expires.getTime())) { appFeedback("请填写有效的凭据到期时间。", true); return; }
      const credentials = { client_id: clientID.value, app_secret: appSecret.value, access_token: accessToken.value };
      // Do not leave credentials in a visible control while the request is pending.
      clientID.value = appSecret.value = accessToken.value = expiresAt.value = "";
      current.bilibiliAuthorizationSaving = true;
      current.bilibiliAuthorizationError = "";
      verify.disabled = reset.disabled = true;
      try {
        const value = await request("/api/local/apps/bilibili/publication-authorization", { method: "POST", body: JSON.stringify({ action: "verify", revision: authorization.revision, ...credentials, expires_at: expires.toISOString(), enabled: enabled.checked }) });
        if (current !== apps) return;
        if (!validPublicationAuthorization(value)) throw new Error("发布授权状态返回格式无效，请刷新后重试。");
        current.bilibiliAuthorization = value;
        clientID.value = appSecret.value = accessToken.value = expiresAt.value = "";
        appFeedback(value.enabled ? "发布授权已核验并登记。" : "发布授权已核验，但目前未启用。");
        await loadApps();
      } catch (error) {
        if (current !== apps || appSessionExpired([{ status: "rejected", reason: error }])) return;
        current.bilibiliAuthorizationError = error.message;
        appFeedback(error.message, true);
      } finally {
        clientID.value = appSecret.value = accessToken.value = expiresAt.value = "";
        current.bilibiliAuthorizationSaving = false;
        verify.disabled = reset.disabled = false;
        if (current === apps) updatePublicationRow(current);
      }
    });
    panel.append(form);
    return panel;
  }
  function publicationDisableButton(current, authorization) {
    if (!authorization || !authorization.enabled) return null;
    const button = appElement("button", current.bilibiliAuthorizationSaving ? "正在停用…" : "停用发布授权", "button compact quiet");
    button.type = "button";
    button.disabled = current.bilibiliAuthorizationSaving;
    button.addEventListener("click", async () => {
      if (current !== apps || current.bilibiliAuthorizationSaving) return;
      current.bilibiliAuthorizationSaving = true;
      updatePublicationRow(current);
      try {
        const value = await request("/api/local/apps/bilibili/publication-authorization", { method: "POST", body: JSON.stringify({ action: "disable", revision: authorization.revision }) });
        if (current !== apps) return;
        if (!validPublicationAuthorization(value)) throw new Error("发布授权状态返回格式无效，请刷新后重试。");
        current.bilibiliAuthorization = value;
        appFeedback("发布授权已停用。");
        await loadApps();
      } catch (error) {
        if (current !== apps || appSessionExpired([{ status: "rejected", reason: error }])) return;
        current.bilibiliAuthorizationError = error.message;
        appFeedback(error.message, true);
      } finally {
        current.bilibiliAuthorizationSaving = false;
        if (current === apps) updatePublicationRow(current);
      }
    });
    return button;
  }
  function bilibiliPublicationRow(current) {
    if (!current.registered?.some((item) => item.id === "bilibili")) return null;
    const work = current.bilibiliWork;
    const authorization = current.bilibiliAuthorization;
    const [title, note] = publicationAuthorizationSummary(authorization);
    const row = appElement("section", "", "app-layer publication-layer");
    row.id = "bilibili-publication";
    row.setAttribute("aria-labelledby", "bilibili-publication-heading");
    row.setAttribute("aria-busy", String(work.reading));
    const heading = appElement("h5", "创作发布");
    heading.id = "bilibili-publication-heading";
    const detail = appElement("div", "", "publication-detail");
    detail.append(appElement("strong", title), appElement("p", note));
    if (authorization?.expires_at) {
      const expiry = new Date(authorization.expires_at);
      if (Number.isFinite(expiry.getTime())) detail.append(appElement("p", `凭据到期：${expiry.toLocaleString()}`, "publication-reference"));
    }
    if (current.bilibiliAuthorizationError) detail.append(appElement("p", current.bilibiliAuthorizationError, "publication-feedback is-warn"));
    if (authorization?.refresh_pending) detail.append(appElement("p", "登录状态需要重新网页授权；不会自动重试刷新。", "publication-feedback is-warn"));
    detail.append(bilibiliOAuthForm(current));
    detail.append(bilibiliAuthorizationForm(current));
    const refresh = publicationRefreshButton(current, authorization);
    if (refresh) detail.append(refresh);
    const disable = publicationDisableButton(current, authorization);
    if (disable) detail.append(disable);
    detail.append(publicationTasksPanel(current, "bilibili", "B 站"));
    row.append(heading, detail);
    return row;
  }
  function douyinPublicationRow(current) {
    if (!current.registered?.some((item) => item.id === "douyin")) return null;
    const tab = current.douyinActivityTab === "interactions" ? "interactions" : "publications";
    const work = tab === "interactions" ? current.douyinInteractions : current.douyinWork;
    const row = appElement("section", "", "app-layer publication-layer");
    row.id = "douyin-publication";
    row.setAttribute("aria-labelledby", "douyin-publication-heading");
    row.setAttribute("aria-busy", String(work.reading));
    const heading = appElement("h5", "作品与互动");
    heading.id = "douyin-publication-heading";
    const detail = appElement("div", "", "publication-detail");
    const tabs = appElement("div", "", "publication-actions douyin-activity-tabs");
    for (const [id, label] of [["publications", "发布记录"], ["interactions", "互动记录"]]) {
      const button = appElement("button", label, `button compact ${tab === id ? "primary" : "quiet"}`);
      button.type = "button";
      button.dataset.activityTab = id;
      button.setAttribute("aria-pressed", String(tab === id));
      button.addEventListener("click", () => {
        if (current !== apps || current.douyinActivityTab === id) return;
        if (current.douyinActivityTab === "interactions") stopDouyinInteractionWork(current);
        current.douyinActivityTab = id;
        updatePublicationRow(current, "douyin");
        if (id === "interactions") void loadDouyinInteractionWork(current, current.douyinInteractions.cursors);
        else void loadPublicationWork(current, "douyin", current.douyinWork.cursors);
      });
      tabs.append(button);
    }
    detail.append(appElement("strong", "应用活动"), appElement("p", tab === "interactions" ? "互动记录只展示已有任务。页面不会自动提交、继续或查询平台。" : "发布记录保留已有创作任务及其结果。新的普通账号发布能力会直接使用已保存的登录状态。"), tabs);
    detail.append(tab === "interactions" ? douyinInteractionTasksPanel(current) : publicationTasksPanel(current, "douyin", "抖音"));
    row.append(heading, detail);
    return row;
  }

  const interactionStates = {
    pending: ["待处理", "等待按原任务提交。"],
    unknown: ["待核对", "平台结果尚未确认；请查询原任务，避免重复操作。"],
    in_progress: ["进行中", "正在处理原互动任务。"],
    applied: ["已完成", "已记录本次互动结果。"],
    failed: ["未完成", "本次互动未完成。"],
    cancelled: ["已停止", "后续互动已停止。"],
  };
  const interactionActions = { submit: "开始互动", resume: "继续任务", status: "查询状态", cancel: "停止后续" };
  function interactionDiagnostic(code) {
    const known = {
      login_required: "抖音网页登录状态需要重新确认，尚未继续互动。",
      profile_busy: "专属网页登录资料仍在使用，尚未继续互动。",
      operation_timeout: "互动页面处理超时，原任务保留待核对。",
      operation_cancelled: "互动页面操作已中断，原任务保留待核对。",
      like_control_unavailable: "点赞控件暂不可用，原任务保留待核对。",
      like_state_unavailable: "无法核对当前点赞状态，原任务保留待核对。",
      reply_parent_unavailable: "无法核对原评论，尚未继续回复。",
      reply_editor_unavailable: "未找到可用的评论回复输入框，尚未继续回复。",
      reply_submit_unavailable: "未找到可用的评论回复提交控件，尚未继续回复。",
      receipt_not_found: "尚未找到平台结果，请查询原任务。",
      receipt_unknown: "平台结果尚未确认，请查询原任务。",
      not_submitted: "互动尚未提交，原任务可继续处理。",
      platform_rejected: "平台没有接受这次互动，原任务保留。",
      authorization_changed: "互动许可或来源资料已变化，未继续操作。",
      browser_unavailable: "浏览器暂不可用，原任务保留。",
      controls_unavailable: "抖音页面控件暂不可用，尚未确认执行。",
      operation_unavailable: "这次互动暂时无法执行，原任务保留。",
      interaction_failed: "这次互动未完成，原任务保留。",
      interaction_cancelled: "这次互动已停止，仍可查询原任务。",
      interaction_rejected: "平台没有接受这次互动，原任务保留。",
      interaction_unknown: "结果尚未确认，请查询原任务。",
      comment_changed: "原评论已变化，未继续回复。",
      comment_unavailable: "无法核对原评论，未继续回复。",
    };
    if (typeof code === "string" && known[code]) return known[code];
    const limit = typeof code === "string" ? /^reply_text_limit_([1-9][0-9]*)$/.exec(code) : null;
    if (limit) return `回复文字超过当前允许的 ${limit[1]} 字，尚未提交。`;
    return code ? "这次互动需要核对原任务。" : "";
  }
  function interactionStatus(task) {
    const normal = interactionStates[task.state] || interactionStates.unknown;
    if (task.cancel_requested && task.state !== "cancelled" && task.state !== "applied" && task.state !== "failed") {
      return ["正在停止", task.state === "unknown" ? "已请求停止，但平台结果仍待核对。" : "已请求停止后续操作。"];
    }
    const diagnostic = interactionDiagnostic(task.diagnostic);
    if (task.cancel_requested && task.state === "applied") {
      return [normal[0], `${diagnostic || normal[1]} 停止请求未撤回这次已核对的操作。`];
    }
    if (task.cancel_requested && task.state === "failed") {
      return [normal[0], `${diagnostic || normal[1]} 停止请求不会撤回此前已发生的操作。`];
    }
    return diagnostic ? [normal[0], diagnostic] : normal;
  }
  function interactionKind(task) {
    if (task.kind === "reply_comment") return "回复评论";
    if (task.kind === "like") {
      if (task.desired === true) return "点赞";
      if (task.desired === false) return "取消点赞";
      return "点赞状态调整";
    }
    return "互动";
  }
  function douyinInteractionTasksPanel(current) {
    return publicationTasksPanel(current, "douyin", "抖音", {
      work: current.douyinInteractions,
      empty: "暂无抖音互动记录。",
      listLabel: "抖音互动记录",
      taskRow: (task) => douyinInteractionTaskRow(current, task),
      load: (cursors) => loadDouyinInteractionWork(current, cursors),
    });
  }
  function publicationTasksPanel(current, appID, label, options = {}) {
    const work = options.work || publicationWork(current, appID);
    const load = options.load || ((cursors) => loadPublicationWork(current, appID, cursors));
    const taskRow = options.taskRow || ((task) => publicationTaskRow(current, task, appID));
    const empty = options.empty || `暂无${label}发布任务。`;
    const listLabel = options.listLabel || `${label}发布任务`;
    const taskPanel = appElement("div", "", "publication-tasks");
    const feedback = appElement("p", work.error || work.notice, work.error || work.actionError ? "publication-feedback is-warn" : "sr-only");
    feedback.setAttribute("role", "status");
    taskPanel.append(feedback);
    const items = work.page?.items || [];
    if (items.length) {
      const list = appElement("ul", "", "publication-list");
      list.setAttribute("aria-label", listLabel);
      for (const task of items) list.append(taskRow(task));
      taskPanel.append(list);
    } else if (!work.error) {
      taskPanel.append(appElement("p", work.reading ? "正在读取记录…" : empty, "publication-empty"));
    }
    if (work.page?.has_more || work.cursors.length > 1) {
      const pages = appElement("div", "", "publication-pages");
      const previous = appElement("button", "上一页", "button compact quiet");
      const next = appElement("button", "下一页", "button compact quiet");
      previous.type = next.type = "button";
      previous.dataset.page = "previous";
      next.dataset.page = "next";
      previous.disabled = work.reading || work.cursors.length === 1;
      next.disabled = work.reading || !work.page?.has_more;
      previous.addEventListener("click", () => load(work.cursors.slice(0, -1)));
      next.addEventListener("click", () => load([...work.cursors, work.page.next]));
      pages.append(previous, appElement("span", `第 ${work.cursors.length} 页`), next);
      taskPanel.append(pages);
    }
    return taskPanel;
  }
  function publicationWork(current, appID) { return appID === "douyin" ? current.douyinWork : current.bilibiliWork; }
  function publicationTaskRow(current, task, appID = "bilibili") {
    const row = appElement("li", "", "publication-task");
    row.dataset.job = task.job_id;
    row.tabIndex = -1;
    const details = appElement("div", "", "publication-task-detail");
    const summary = appElement("div", "", "publication-task-summary");
    const [title, note] = publicationStatus(task);
    const badge = appElement("strong", title, "publication-status");
    badge.dataset.state = task.state;
    const identifier = appElement("span", task.remote_id || `任务 ${task.job_id.slice(0, 10)}`, "publication-reference");
    identifier.title = task.job_id;
    summary.append(badge, identifier);
    if (task.updated_at) {
      const value = new Date(task.updated_at);
      if (Number.isFinite(value.getTime())) {
        const time = appElement("time", value.toLocaleString(), "publication-reference");
        time.dateTime = task.updated_at;
        summary.append(time);
      }
    }
    details.append(summary);
    const progress = task.progress;
    if (Number.isSafeInteger(progress?.total_parts) && progress.total_parts > 0 &&
      Number.isSafeInteger(progress.confirmed_parts) && progress.confirmed_parts >= 0 &&
      progress.confirmed_parts <= progress.total_parts) {
      const meter = appElement("progress", "", "publication-meter");
      meter.max = progress.total_parts;
      meter.value = progress.confirmed_parts;
      const caption = `已确认 ${meter.value} / ${meter.max} 段`;
      meter.setAttribute("aria-label", caption);
      const line = appElement("div", "", "publication-progress");
      line.append(meter, appElement("span", caption));
      details.append(line);
    }
    const pending = publicationWork(current, appID).pending.get(task.job_id);
    const pendingLabel = pending?.has("cancel") ? "正在登记停止…" : "正在处理原任务…";
    details.append(appElement("p", pending?.size ? pendingLabel : note));
    const buttons = appElement("div", "", "publication-actions");
    const available = Array.isArray(task.available_actions) ? task.available_actions : [];
    for (const [action, label] of Object.entries(publicationActions)) {
      if (!available.includes(action)) continue;
      if (action === "resume" && task.progress?.stage === "part_pending") continue;
      const button = appElement("button", label, `button compact ${action === "submit" ? "primary" : "quiet"}`);
      button.type = "button";
      button.dataset.action = action;
      // Cancellation remains possible while an upload request is running.
      button.disabled = Boolean(pending?.has(action) || (pending?.size && action !== "cancel"));
      button.addEventListener("click", () => applyPublicationAction(current, task, action, appID));
      buttons.append(button);
    }
    if (appID === "bilibili" && available.includes("reconcile") && task.state === "awaiting_receipt" && task.progress?.stage === "posting" && !task.remote_id) {
      details.append(publicationReconciliation(current, task));
    }
    row.append(details, buttons);
    return row;
  }
  function douyinInteractionTaskRow(current, task) {
    const row = appElement("li", "", "publication-task");
    row.dataset.job = task.job_id;
    row.tabIndex = -1;
    const details = appElement("div", "", "publication-task-detail");
    const summary = appElement("div", "", "publication-task-summary");
    const [title, note] = interactionStatus(task);
    const badge = appElement("strong", title, "publication-status");
    badge.dataset.state = task.state === "applied" ? "published" : task.state === "unknown" ? "awaiting_receipt" : task.state;
    const kind = appElement("span", interactionKind(task), "publication-reference");
    const identifier = appElement("span", task.remote_id || `任务 ${String(task.job_id || "").slice(0, 10)}`, "publication-reference");
    identifier.title = task.job_id || "";
    summary.append(badge, kind, identifier);
    if (task.updated_at) {
      const value = new Date(task.updated_at);
      if (Number.isFinite(value.getTime())) {
        const time = appElement("time", value.toLocaleString(), "publication-reference");
        time.dateTime = task.updated_at;
        summary.append(time);
      }
    }
    details.append(summary, appElement("p", note));
    const pending = current.douyinInteractions.pending.get(task.job_id);
    if (pending?.size) details.append(appElement("p", pending.has("cancel") ? "正在登记停止…" : "正在处理原互动任务…", "publication-reference"));
    const buttons = appElement("div", "", "publication-actions");
    const available = Array.isArray(task.available_actions) ? task.available_actions : [];
    for (const [action, label] of Object.entries(interactionActions)) {
      if (!available.includes(action)) continue;
      const button = appElement("button", label, `button compact ${action === "submit" ? "primary" : "quiet"}`);
      button.type = "button";
      button.dataset.action = action;
      // Stopping remains usable while a previous submit/status request is in flight.
      button.disabled = Boolean(pending?.has(action) || (pending?.size && action !== "cancel"));
      button.addEventListener("click", () => { void applyDouyinInteractionAction(current, task, action); });
      buttons.append(button);
    }
    row.append(details, buttons);
    return row;
  }
  function validBilibiliBVID(value) { return typeof value === "string" && /^BV[0-9A-Za-z]{10}$/.test(value); }
  function publicationReconciliation(current, task) {
    const work = current.bilibiliWork;
    const state = work.reconciliation.get(task.job_id) || { candidates: null, page: 1, nextPage: 0, hasMore: false, bvid: "", busy: false, error: "" };
    work.reconciliation.set(task.job_id, state);
    const panel = appElement("details", "", "publication-reconciliation");
    panel.open = state.busy || Boolean(state.error) || Array.isArray(state.candidates);
    panel.append(appElement("summary", "核对投稿结果"));
    const body = appElement("div", "", "publication-reconciliation-body");
    const saved = task.reconciliation;
    if (validBilibiliBVID(saved?.bvid)) {
      const checked = saved.checked_at && Number.isFinite(new Date(saved.checked_at).getTime()) ? `，核对于 ${new Date(saved.checked_at).toLocaleString()}` : "";
      body.append(appElement("p", `已核对 BV 号 ${saved.bvid}${saved.published ? "，平台显示已公开" : "，平台尚未显示公开"}${checked}。这条观察不会改变原投稿任务状态。`, "publication-reference"));
    } else {
      body.append(appElement("p", "候选稿件仅供核对。选择候选或粘贴已知 BV 号后，任务仍保持待核对。", "publication-reference"));
    }
    const actions = appElement("div", "", "publication-actions");
    const discover = appElement("button", state.busy ? "正在查找…" : "查找候选稿件", "button compact quiet");
    discover.type = "button"; discover.disabled = state.busy;
    discover.addEventListener("click", () => reconcilePublication(current, task, "candidates", 1));
    const bvid = document.createElement("input");
    bvid.type = "text"; bvid.autocomplete = "off"; bvid.placeholder = "粘贴 BV 号"; bvid.value = state.bvid; bvid.setAttribute("aria-label", `任务 ${task.job_id.slice(0, 10)} 的 BV 号`);
    bvid.addEventListener("input", () => { state.bvid = bvid.value.trim(); });
    const verify = appElement("button", "核对 BV 号", "button compact quiet");
    verify.type = "button"; verify.disabled = state.busy;
    verify.addEventListener("click", () => reconcilePublication(current, task, "verify", 0, bvid.value.trim()));
    actions.append(discover, bvid, verify);
    body.append(actions);
    if (state.error) body.append(appElement("p", state.error, "publication-feedback is-warn"));
    if (Array.isArray(state.candidates)) {
      const candidates = appElement("ul", "", "publication-reconciliation-list");
      for (const item of state.candidates) {
        if (!validBilibiliBVID(item?.bvid)) continue;
        const row = appElement("li", "", "publication-reconciliation-item");
        const created = item.created_at && Number.isFinite(new Date(item.created_at).getTime()) ? `稿件创建时间 ${new Date(item.created_at).toLocaleString()}` : "";
        const text = [item.bvid, typeof item.title === "string" && item.title.trim() ? item.title.trim() : "未提供标题", created].filter(Boolean).join(" · ");
        row.append(appElement("span", text));
        const check = appElement("button", "核对此稿件", "button compact quiet");
        check.type = "button"; check.disabled = state.busy;
        check.addEventListener("click", () => reconcilePublication(current, task, "verify", 0, item.bvid));
        row.append(check);
        candidates.append(row);
      }
      if (!candidates.childElementCount) candidates.append(appElement("li", "暂未找到可核对的候选稿件。", "publication-reference"));
      body.append(candidates);
      if (state.hasMore && Number.isSafeInteger(state.nextPage) && state.nextPage > state.page) {
        const next = appElement("button", `继续查找第 ${state.nextPage} 页`, "button compact quiet");
        next.type = "button"; next.disabled = state.busy;
        next.addEventListener("click", () => reconcilePublication(current, task, "candidates", state.nextPage));
        body.append(next);
      }
    }
    panel.append(body);
    return panel;
  }
  async function reconcilePublication(current, task, action, page = 1, bvid = "") {
    if (current !== apps) return;
    const work = current.bilibiliWork;
    const state = work.reconciliation.get(task.job_id);
    if (!state || state.busy) return;
    if (action === "verify") state.bvid = bvid;
    if (action === "verify" && !validBilibiliBVID(bvid)) {
      state.error = "请输入完整的 BV 号后再核对。";
      updatePublicationRow(current);
      return;
    }
    state.busy = true; state.error = "";
    updatePublicationRow(current);
    try {
      const body = action === "candidates" ? { job_id: task.job_id, action, page, page_size: 10 } : { job_id: task.job_id, action, bvid };
      const result = await request("/api/local/publications/reconcile", { method: "POST", body: JSON.stringify(body) });
      if (current !== apps || result.job_id !== task.job_id) return;
      if (action === "candidates") {
        if (!Array.isArray(result.items) || !Number.isSafeInteger(result.page) || !Number.isSafeInteger(result.page_size)) throw new Error("本机返回的候选稿件格式无效。");
        state.candidates = result.items; state.page = result.page; state.nextPage = result.next_page; state.hasMore = result.has_more === true;
      } else if (result.item?.bvid !== bvid || !validBilibiliBVID(result.item?.bvid) || typeof result.item.published !== "boolean") {
        throw new Error("本机返回的稿件核对结果无效。");
      } else {
        work.notice = `已核对 ${result.item.bvid}。原投稿任务仍待核对。`;
        await loadBilibiliWork(current, work.cursors);
      }
    } catch (error) {
      if (current === apps && !appSessionExpired([{ status: "rejected", reason: error }])) state.error = error.message;
    } finally {
      if (current === apps) { state.busy = false; updatePublicationRow(current); }
    }
  }
  function updatePublicationRow(current, appID = "bilibili") {
    if (current !== apps) return;
    const row = $(`#${appID}-publication`);
    if (!row) return;
    const active = document.activeElement;
    const focusedJob = row.contains(active) ? active.closest("[data-job]")?.dataset.job : "";
    const focusedAction = active?.dataset.action;
    const focusedPage = row.contains(active) ? active.dataset.page : "";
    let next = appID === "douyin" ? douyinPublicationRow(current) : bilibiliPublicationRow(current);
    if (!next) return;
    if (appID === "bilibili" && row.querySelector(".publication-authorization[open], .publication-oauth[open]")) {
      // Refresh tasks without touching the open credential form.
      row.querySelector(".publication-tasks").replaceWith(next.querySelector(".publication-tasks"));
      row.setAttribute("aria-busy", next.getAttribute("aria-busy"));
      next = row;
    } else {
      row.replaceWith(next);
    }
    if (focusedJob) {
      const task = Array.from(next.querySelectorAll("[data-job]")).find((element) => element.dataset.job === focusedJob);
      const button = Array.from(task?.querySelectorAll("button:not(:disabled)") || []).find((element) => element.dataset.action === focusedAction);
      (button || task)?.focus({ preventScroll: true });
    } else if (focusedPage) {
      const buttons = Array.from(next.querySelectorAll("[data-page]"));
      const button = buttons.find((element) => element.dataset.page === focusedPage && !element.disabled);
      (button || buttons.find((element) => !element.disabled) || next.querySelector("[data-job]"))?.focus({ preventScroll: true });
    }
  }
  async function loadPublicationWork(current, appID, cursors) {
    const work = publicationWork(current, appID);
    const readID = ++work.readID;
    work.reading = true;
    work.error = "";
    updatePublicationRow(current, appID);
    try {
      const after = cursors.at(-1) || 0;
      const result = await request(`/api/local/publications?app_id=${encodeURIComponent(appID)}&limit=6&after=${after}`);
      if (current !== apps || readID !== work.readID) return;
      if (!Array.isArray(result.tasks?.items)) throw new Error("本机未返回发布任务列表。");
      work.page = result.tasks;
      work.cursors = cursors;
    } catch (error) {
      if (current !== apps || readID !== work.readID || appSessionExpired([{ status: "rejected", reason: error }])) return;
      work.error = error.message;
    } finally {
      if (current === apps && readID === work.readID) {
        work.reading = false;
        updatePublicationRow(current, appID);
      }
    }
  }
  async function loadDouyinInteractionWork(current, cursors) {
    const work = current?.douyinInteractions;
    if (!work || current !== apps || current.openID !== "douyin" || current.douyinActivityTab !== "interactions") return;
    const readID = ++work.readID;
    const epoch = work.epoch;
    work.reading = true;
    work.error = "";
    updatePublicationRow(current, "douyin");
    try {
      const after = cursors.at(-1) || 0;
      const result = await request(`/api/local/interactions?app_id=douyin&limit=6&after=${after}`);
      if (current !== apps || work.epoch !== epoch || readID !== work.readID || current.openID !== "douyin" || current.douyinActivityTab !== "interactions") return;
      if (!Array.isArray(result.tasks?.items)) throw new Error("本机未返回互动记录。");
      work.page = result.tasks;
      work.cursors = cursors;
    } catch (error) {
      // Only an active page may surface a permission failure. A closed/stale
      // detail response must not overwrite the next owner view or log out.
      if (current !== apps || work.epoch !== epoch || readID !== work.readID || current.openID !== "douyin" || current.douyinActivityTab !== "interactions" || appSessionExpired([{ status: "rejected", reason: error }])) return;
      work.error = error.message || "互动记录暂不可读。";
    } finally {
      if (current === apps && work.epoch === epoch && readID === work.readID && current.openID === "douyin" && current.douyinActivityTab === "interactions") {
        work.reading = false;
        updatePublicationRow(current, "douyin");
      }
    }
  }
  async function loadBilibiliWork(current, cursors) { return loadPublicationWork(current, "bilibili", cursors); }
  async function applyPublicationAction(current, task, action, appID = "bilibili") {
    if (current !== apps || !task.available_actions?.includes(action)) return;
    if (action === "resume" && task.progress?.stage === "part_pending") return;
    const work = publicationWork(current, appID);
    const pending = work.pending.get(task.job_id) || new Set();
    if (pending.has(action) || (pending.size && action !== "cancel")) return;
    pending.add(action);
    work.pending.set(task.job_id, pending);
    work.notice = "";
    work.actionError = false;
    updatePublicationRow(current, appID);
    try {
      const updated = await request("/api/local/publications", { method: "POST", body: JSON.stringify({ action, job_id: task.job_id }) });
      if (current === apps && updated.job_id === task.job_id) work.notice = publicationStatus(updated)[1];
    } catch (error) {
      if (current !== apps || appSessionExpired([{ status: "rejected", reason: error }])) return;
      work.notice = error.message;
      work.actionError = true;
    } finally {
      pending.delete(action);
      if (!pending.size) work.pending.delete(task.job_id);
      if (current === apps) await loadPublicationWork(current, appID, work.cursors);
    }
  }
  async function applyDouyinInteractionAction(current, task, action) {
    if (current !== apps || current.openID !== "douyin" || current.douyinActivityTab !== "interactions" || !task.available_actions?.includes(action)) return;
    const work = current.douyinInteractions;
    const pending = work.pending.get(task.job_id) || new Set();
    if (pending.has(action) || (pending.size && action !== "cancel")) return;
    const epoch = work.epoch;
    pending.add(action);
    work.pending.set(task.job_id, pending);
    work.notice = "";
    work.actionError = false;
    updatePublicationRow(current, "douyin");
    try {
      const updated = await request("/api/local/interactions", { method: "POST", body: JSON.stringify({ action, job_id: task.job_id }) });
      if (current !== apps || work.epoch !== epoch || current.openID !== "douyin" || current.douyinActivityTab !== "interactions" || updated.job_id !== task.job_id) return;
      work.notice = interactionStatus(updated)[1];
    } catch (error) {
      // 403 is the current task's permission change, not a session logout.
      if (current !== apps || work.epoch !== epoch || current.openID !== "douyin" || current.douyinActivityTab !== "interactions" || appSessionExpired([{ status: "rejected", reason: error }])) return;
      work.notice = error.message || "这次互动未完成。";
      work.actionError = true;
    } finally {
      // A close/reopen replaces the Map. Never let an old request clear a
      // fresh action for the same task; it may only repaint the current view.
      if (work.pending.get(task.job_id) === pending) {
        pending.delete(action);
        if (!pending.size) work.pending.delete(task.job_id);
      }
      if (current === apps && work.epoch === epoch && current.openID === "douyin" && current.douyinActivityTab === "interactions") await loadDouyinInteractionWork(current, work.cursors);
    }
  }
  async function installApp(listing) {
    const current = apps;
    if (current.loading || current.installing || !state.home?.home_access) return;
    current.installing = listing.id;
    appFeedback("");
    renderApps();
    try {
      const result = await request(`/api/local/apps/${encodeURIComponent(listing.id)}/install`, {
        method: "POST", body: JSON.stringify({ digest: listing.digest, revision: state.home.revision })
      });
      if (current !== apps) return;
      appFeedback(result.result === "existing" ? "接入版本已在家目录中。" : "接入版本已安装到家目录。");
      await loadApps();
    } catch (error) {
      if (current !== apps) return;
      if (error.status === 401 || error.status === 403) {
        state.csrf = "";
        showLogin();
        showMessage("登录状态已失效，请重新验证。", "warn");
      } else {
        appFeedback(error.message, true);
        if (error.status === 409) {
          const home = await request("/api/local/home").catch(() => null);
          if (home && current === apps) showHome(home);
          if (current === apps) await loadApps();
        }
      }
    } finally {
      if (current === apps) {
        current.installing = "";
        renderApps();
      }
    }
  }
  function projectToolStatus(tool) {
    if (!tool || !tool.installed && !tool.catalog_available) return ["未随当前发行包提供", "请安装包含此工具的 Windows 发行包。"];
    if (tool && tool.id === "obs-preview") {
      const preview = projectTools.obsPreview;
      if (!tool.installed) return ["尚未安装", "安装后可登记本机 OBS WebSocket 凭据，并按需查看当前节目画面。"];
      if (!tool.enabled && preview.configured) return ["已配置，未启用", "启用后可以按需开始预览。"];
      if (!tool.enabled) return ["已安装，待配置", "保存本机 OBS 回环端口和密码，然后启用预览工具。"];
      if (!preview.configured) return ["等待配置", "请先停用工具，保存本机 OBS 回环端口和密码。"];
      if (preview.active) return ["正在预览", "正在读取 OBS 当前节目画面；离开此页或 60 秒无操作后会自动停止。"];
      return ["已启用 · 等待预览", "需要时开始查看当前节目画面；不会开播、切换场景或写入 OBS。"];
    }
    if (tool && tool.id === "rvc") {
      if (!tool.runtime_ready) return ["本机后端未就绪", projectToolReason(tool) || "RVC 换声后端尚不能启动；请先补齐本机运行环境。"];
      if (!tool.installed) return ["尚未安装", "安装后可在本地聊天中，对获准音频选择已登记声线进行换声。"];
      if (!tool.enabled) return ["已安装，未启用", "启用后仅在实际换声任务中按需启动，不会自动处理或发送音频。"];
      if (tool.state === "running") return ["任务处理中", "可在本地聊天中按原任务编号核对进度。"];
      if (tool.state === "failed") return ["本机后端异常", projectToolReason(tool) || "下次换声任务会重新检查本机后端。"];
      return ["已启用，等待任务", "收到明确的换声请求后，才会读取本次获准音频和所选声线。"];
    }
    if (tool && tool.id === "comfyui") {
      const workflows = comfyWorkflows(tool);
      if (tool.reason === "comfyui_workflows_missing") return ["工作流未就绪", "请登记至少一个通过校验的工作流，再启用 ComfyUI。"];
      if (!tool.runtime_ready) return ["ComfyUI 本机后端未就绪", projectToolReason(tool) || "尚未找到可启动的 ComfyUI 本机后端。"];
      if (!tool.installed) return ["尚未安装", "安装后可在本地聊天中按任务调用已登记的图像工作流。"];
      if (!workflows.length) return ["未登记工作流", projectToolReason(tool) || "请先在工具包中登记至少一个可用工作流；不会把空配置当作可用。"];
      if (!tool.enabled) return ["已安装，未启用", "启用后可按需运行已登记的图像工作流。"];
      if (tool.state === "running") return ["任务处理中", "正在按本次任务调用已登记工作流；成品写入完成后会回到待命。"];
      if (tool.state === "failed") return ["ComfyUI 本机后端异常", projectToolReason(tool) || "下次图像任务会重新检查本机后端和工作流。"];
      return ["已启用，等待任务", "本地聊天需要制作图像时会按需启动已登记的工作流。"];
    }
    if (tool && tool.id === "yue") {
      if (!tool.runtime_ready) return ["YuE 本机后端未就绪", projectToolReason(tool) || "尚未登记可启动的官方 YuE 本机后端。"];
      if (!tool.installed) return ["尚未安装", "安装后可在本地聊天中按任务创作原创或翻唱音乐。"];
      if (!tool.enabled) return ["已安装，未启用", "启用后会在明确的作曲任务中按需启动，不会自行生成或发送音乐。"];
      if (tool.state === "running") return ["任务处理中", "正在按本次任务调用官方 YuE 本机后端；成品核验后会回到待命。"];
      if (tool.state === "failed") return ["YuE 本机后端异常", projectToolReason(tool) || "下次作曲任务会重新检查本机后端。"];
      return ["已启用，等待任务", "本地聊天需要原创或翻唱音乐时会按需启动已登记的 YuE 后端。"];
    }
    if (tool && tool.id === "station-speech") {
      if (!tool.runtime_ready) return ["本机语音后端未就绪", projectToolReason(tool) || "尚未登记可启动的本机语音后端或可用声线。"];
      if (!tool.installed) return ["尚未安装", "安装后可在本地聊天中使用已登记声线合成语音。"];
      if (!tool.enabled) return ["已安装，未启用", "启用后仅在明确的语音合成任务中按需启动，不会自动发送语音。"];
      if (tool.state === "running") return ["任务处理中", "正在按本次任务使用指定声线合成语音；成品核验后会回到待命。"];
      if (tool.state === "failed") return ["本机语音后端异常", projectToolReason(tool) || "下次语音任务会重新检查本机后端。"];
      return ["已启用，等待任务", "本地聊天需要语音时会按需启动已登记的本机语音后端。"];
    }
    if (!tool.runtime_ready) return ["本地环境未就绪", projectToolReason(tool) || "请先补齐本机运行环境或依赖包。"];
    if (!tool.installed) return ["尚未安装", "安装后才会把工具放入大肥鱼的项目环境。"];
    if (!tool.enabled && tool.update_available) return ["已安装，可更新", "已发现随当前发行包提供的新版本；更新会保留已登记的本地成品。"];
    if (!tool.enabled) return ["已安装，未启用", "需要时启用；停用不会删除已生成的本地成品。"];
    if (tool.state === "running") return ["已启动", tool.id === "audio-workshop" ? "本次音频处理结束后关闭运行器。" : "本地运行器空闲一段时间后会自动关闭。"];
    if (tool.state === "failed") return ["运行异常", "下次有本地任务时会尝试重新启动。"];
    return ["已启用，待命", "会在本地聊天实际需要时启动，不会取得网络或应用账号权限。"];
  }
  function projectToolReason(tool) {
    const reasons = {
      rvc_backend_missing: "当前发行包未找到 RVC 后端。",
      rvc_backend_not_configured: "尚未登记本机 RVC 后端或可用声线。",
      rvc_backend_unavailable: "本机 RVC 后端暂不可用，可检查本机运行环境后再试。",
      rvc_backend_start_failed: "RVC 后端启动失败，下一次换声任务会重新检查。",
      rvc_voice_profiles_missing: "尚未登记可选声线，暂时不能发起换声任务。",
      comfyui_backend_missing: "当前发行包未找到 ComfyUI 本机后端。",
      comfyui_backend_not_configured: "尚未登记可启动的 ComfyUI 本机后端。",
      comfyui_backend_unavailable: "ComfyUI 本机后端暂不可用，可检查运行环境后再试。",
      comfyui_backend_start_failed: "ComfyUI 后端启动失败，下一次图像任务会重新检查。",
      comfyui_workflows_missing: "尚未登记可调用的 ComfyUI 工作流。",
      comfyui_workflow_catalog_missing: "当前工具包没有可用的 ComfyUI 工作流清单。",
      comfyui_workflow_invalid: "已登记的 ComfyUI 工作流无效，需要先修正工具配置。",
      yue_backend_missing: "当前发行包未找到 YuE 本机后端适配。",
      yue_backend_not_configured: "尚未登记可启动的官方 YuE 本机后端。",
      yue_backend_unavailable: "YuE 本机后端暂不可用，可检查本机运行环境后再试。",
      yue_backend_start_failed: "YuE 后端启动失败，下一次作曲任务会重新检查。",
      station_speech_backend_not_configured: "尚未登记可启动的本机语音后端或可用声线。",
      station_speech_backend_unavailable: "本机语音后端暂不可用，可检查本机运行环境后再试。",
      station_speech_backend_start_failed: "本机语音后端启动失败，下一次语音任务会重新检查。"
    };
    const code = typeof tool?.reason === "string" ? tool.reason.trim() : "";
    if (reasons[code]) return reasons[code];
    if (/^[\u4e00-\u9fff\u3002\uff0c\uff1a\uff1b\uff01\uff1f\u3001\sA-Za-z0-9_-]{1,120}$/.test(code) && /[\u4e00-\u9fff]/.test(code)) return code;
    if (!code) return "";
    if (tool?.id === "rvc") return "本机 RVC 状态暂不可用，请查看工具配置后再试。";
    if (tool?.id === "comfyui") return "本机 ComfyUI 状态暂不可用，请检查本机后端和已登记工作流。";
    if (tool?.id === "yue") return "本机 YuE 状态暂不可用，请检查已登记的官方本机后端。";
    if (tool?.id === "station-speech") return "本机语音状态暂不可用，请检查已登记的本机后端和声线。";
    return "本机工具状态暂不可用，请查看工具配置后再试。";
  }
  function comfyWorkflows(tool) {
    return Array.isArray(tool?.workflows) ? tool.workflows.filter((workflow) => workflow && typeof workflow.id === "string" && workflow.id.trim() && typeof workflow.label === "string" && workflow.label.trim()) : [];
  }
  function newDeploymentState() {
    return { loading: false, saving: false, loaded: false, generation: 0, server: null, draft: null, dirty: false, revision: null, approved: false, advancedOpen: false, error: "", notice: "" };
  }
  function deploymentDescriptor(id) {
    if (id === "comfyui") return { state: "comfyDeployment", name: "ComfyUI", prefix: "comfy", port: 8190, idle: 300, idleMinutes: 5 };
    if (id === "yue") return { state: "yueDeployment", name: "YuE", prefix: "yue", port: 8191, idle: 0, idleMinutes: 20 };
    return null;
  }
  function normalizeDeployment(value, id = "comfyui") {
    const spec = deploymentDescriptor(id), source = value && typeof value === "object" ? value : {};
    const defaults = { runtime: "", python: "python_embeded/python.exe", entry: "ComfyUI/main.py", frontend: "python_embeded/Lib/site-packages/comfyui_frontend_package/static", manifest: "", models: "", workflows: "", ffmpeg: "" };
    const text = (key) => typeof source[key] === "string" ? source[key] : defaults[key];
    const number = (key, fallback) => Object.hasOwn(source, key) && source[key] !== undefined && source[key] !== null ? source[key] : fallback;
    const configuration = {
      schema_version: 1, enabled: source.enabled !== false, runtime: text("runtime"), python: text("python"), entry: text("entry"),
      frontend: text("frontend"), manifest: text("manifest"), models: text("models"), workflows: text("workflows"),
      port: number("port", spec.port), idle_seconds: number("idle_seconds", spec.idle),
      device: source.device === "cuda" ? "cuda" : "cpu", cuda_device: number("cuda_device", 0)
    };
    if (id === "yue") {
      configuration.ffmpeg = text("ffmpeg");
      if (source.workflow_pins && typeof source.workflow_pins === "object" && !Array.isArray(source.workflow_pins)) configuration.workflow_pins = { ...source.workflow_pins };
      if (typeof source.ffmpeg_sha256 === "string" && source.ffmpeg_sha256) configuration.ffmpeg_sha256 = source.ffmpeg_sha256;
    }
    return configuration;
  }
  function validateDeploymentDraft(draft, id) {
    const configuration = normalizeDeployment(draft, id);
    const integer = (key, minimum, maximum, label) => {
      const raw = configuration[key], value = typeof raw === "string" && !raw.trim() ? NaN : Number(raw);
      if (!Number.isInteger(value) || value < minimum || value > maximum) return `${label}请输入 ${minimum}–${maximum} 的整数。`;
      configuration[key] = value;
      return "";
    };
    return integer("port", 1024, 65535, "回环端口") || integer("idle_seconds", 0, 9223372036, "闲置停止秒数") || integer("cuda_device", 0, 64, "CUDA 设备编号") || configuration;
  }
  function normalizeLocalVision(configuration) {
    const value = configuration || {};
    return { endpoint: String(value.endpoint || ""), model: String(value.model || ""), backend: value.backend === "deepseek" ? "deepseek" : "openai-compatible", max_tokens: value.max_tokens ?? 1024, thinking: value.thinking === true, reasoning_effort: ["low", "high", "max"].includes(value.reasoning_effort) ? value.reasoning_effort : "low" };
  }
  function validLocalVision(view) {
    return view && Number.isSafeInteger(view.revision) && view.revision >= 0 && view.configuration && typeof view.configuration === "object" && typeof view.key_configured === "boolean" && typeof view.ready === "boolean";
  }
  function localVisionSelected(current) { return current === projectTools && current.openID === "media-understanding" && $("#tool-dialog").open && current.tools.some((tool) => tool.id === "media-understanding" && tool.installed); }
  function updateLocalVisionSave(current = projectTools) {
    const button = $("#local-vision-save");
    if (!button) return;
    const vision = current.vision;
    button.hidden = !localVisionSelected(current);
    button.textContent = vision.saving ? "正在保存…" : "保存视觉设置";
    button.disabled = vision.loading || vision.saving || !vision.server || !vision.approved;
  }
  async function loadLocalVision(current = projectTools) {
    if (!localVisionSelected(current) || current.vision.loading || current.vision.saving) return;
    const vision = current.vision, generation = ++vision.generation;
    vision.loading = true; vision.error = "";
    renderProjectToolDialog(current.tools.find((tool) => tool.id === "media-understanding"));
    try {
      const view = await request("/api/local/project-tools/media-understanding/vision");
      if (!localVisionSelected(current) || vision.generation !== generation) return;
      if (!validLocalVision(view)) throw new Error("视觉设置返回格式无效，请刷新后重试。");
      vision.server = view;
      if (!vision.draft) vision.draft = normalizeLocalVision(view.configuration);
    } catch (error) {
      if (!localVisionSelected(current) || vision.generation !== generation || appSessionExpired([{ status: "rejected", reason: error }])) return;
      vision.error = error.message || "视觉设置暂时无法读取。";
    } finally {
      if (current === projectTools && vision.generation === generation) {
        vision.loading = false;
        if (localVisionSelected(current)) renderProjectToolDialog(current.tools.find((tool) => tool.id === "media-understanding"));
      }
    }
  }
  async function saveLocalVision(current = projectTools) {
    const vision = current.vision;
    if (!localVisionSelected(current) || vision.loading || vision.saving || !vision.approved || !vision.server || !vision.draft) return;
    const configuration = normalizeLocalVision(vision.draft);
    configuration.max_tokens = Number(configuration.max_tokens);
    if (!Number.isSafeInteger(configuration.max_tokens) || configuration.max_tokens < 1) {
      vision.error = "输出上限请输入正整数。";
      renderProjectToolDialog(current.tools.find((tool) => tool.id === "media-understanding")); return;
    }
    const generation = ++vision.generation;
    vision.saving = true; vision.error = ""; vision.notice = "";
    renderProjectToolDialog(current.tools.find((tool) => tool.id === "media-understanding"));
    try {
      const view = await request("/api/local/project-tools/media-understanding/vision", { method: "PUT", body: JSON.stringify({ revision: vision.server.revision, configuration, api_key: vision.apiKey, clear_key: vision.clearKey, approve_model: true }) });
      if (!localVisionSelected(current) || vision.generation !== generation) return;
      if (!validLocalVision(view)) throw new Error("视觉设置返回格式无效，请刷新后核对。");
      vision.server = view; vision.draft = normalizeLocalVision(view.configuration);
      vision.apiKey = ""; vision.clearKey = false; vision.approved = false;
      vision.notice = "视觉设置已保存，未调用模型；语音识别配置保持不变。";
    } catch (error) {
      if (!localVisionSelected(current) || vision.generation !== generation || appSessionExpired([{ status: "rejected", reason: error }])) return;
      vision.error = error.message || "视觉设置未保存。";
    } finally {
      if (current === projectTools && vision.generation === generation) {
        vision.saving = false;
        if (localVisionSelected(current)) renderProjectToolDialog(current.tools.find((tool) => tool.id === "media-understanding"));
      }
    }
  }
  function renderLocalVision(tool) {
    const current = projectTools, vision = current.vision;
    const section = appElement("section", "", "local-vision-settings");
    const heading = appElement("div", "", "local-vision-heading");
    heading.append(appElement("strong", "视觉观察"), appElement("span", vision.server?.ready ? "已配置 · 按需调用" : vision.server?.reason || "为本机大肥鱼单独配置视觉服务。"));
    section.append(heading);
    if (vision.error) section.append(appElement("p", vision.error, "local-vision-error"));
    if (vision.notice) section.append(appElement("p", vision.notice, "local-vision-note"));
    if (!vision.server) {
      section.append(appElement("p", vision.loading ? "正在读取视觉设置…" : "视觉设置暂不可读，可点击顶部刷新。", "local-vision-note")); return section;
    }
    const draft = vision.draft || (vision.draft = normalizeLocalVision(vision.server.configuration));
    const form = appElement("form", "", "local-vision-form"); form.noValidate = true;
    const busy = vision.loading || vision.saving;
    const fields = [
      ["接口类型", "backend", { select: [["openai-compatible", "OpenAI 兼容"], ["deepseek", "DeepSeek"]] }],
      ["视觉模型", "model", {}], ["接口地址", "endpoint", { type: "url", placeholder: "https://example.com/v1/chat/completions" }],
      ["输出上限", "max_tokens", { type: "number", min: 1, inputMode: "numeric" }],
      ["思考强度", "reasoning_effort", { select: [["low", "低"], ["high", "高"], ["max", "最高"]] }]
    ];
    for (const [label, key, options] of fields) {
      const field = comfyDeploymentField(label, key, draft, { ...options, disabled: busy || key === "reasoning_effort" && !draft.thinking });
      field.input.id = `local-vision-${key}`; field.label.htmlFor = field.input.id;
      field.input.setAttribute("aria-label", label);
      field.input.addEventListener("input", () => { draft[key] = field.input.value; vision.error = ""; vision.notice = ""; });
      form.append(field.label);
    }
    const keyLabel = appElement("label", "服务密钥");
    const key = document.createElement("input"); key.type = "password"; key.autocomplete = "new-password"; key.id = "local-vision-key";
    key.value = vision.apiKey; key.placeholder = vision.server.key_configured ? "已配置，留空保留" : "填写此服务的密钥"; key.disabled = busy || vision.clearKey;
    key.addEventListener("input", () => { vision.apiKey = key.value; }); keyLabel.append(key); form.append(keyLabel);
    const switches = appElement("div", "", "local-vision-switches");
    for (const [label, checked, onChange] of [
      ["开启思考", draft.thinking, (value) => { draft.thinking = value; renderProjectToolDialog(tool); }],
      ["清除已保存密钥", vision.clearKey, (value) => { vision.clearKey = value; if (value) vision.apiKey = ""; renderProjectToolDialog(tool); }],
      ["允许使用此视觉服务", vision.approved, (value) => { vision.approved = value; updateLocalVisionSave(current); }]
    ]) {
      const labelNode = appElement("label", "", "project-tool-approval"), input = document.createElement("input"); input.type = "checkbox"; input.checked = checked; input.disabled = busy;
      input.addEventListener("change", () => onChange(input.checked)); labelNode.append(input, document.createTextNode(label)); switches.append(labelNode);
    }
    form.append(switches); form.addEventListener("submit", (event) => { event.preventDefault(); void saveLocalVision(current); });
    section.append(form, appElement("p", "保存不会发起测试调用；仅在实际视觉任务中使用。", "local-vision-note")); return section;
  }
  function deploymentSelected(current, id) {
    return current === projectTools && current.openID === id && $("#tool-dialog").open && current.tools.some((tool) => tool.id === id);
  }
  function validDeploymentView(view) {
    return view && typeof view.revision === "string" && view.revision.length > 0 && typeof view.configured === "boolean" && typeof view.read_only === "boolean" && (view.configuration === null || typeof view.configuration === "object" && !Array.isArray(view.configuration));
  }
  function updateDeploymentSave(current = projectTools) {
    const button = $("#local-deployment-save");
    if (!button) return;
    if (current.openID === "obs-preview") {
      const deployment = current.obsDeployment;
      button.hidden = !obsDeploymentSelected(current) || deployment.server?.installed === true;
      button.textContent = deployment.saving ? "正在安装…" : "安装专用输出";
      button.disabled = deployment.loading || deployment.saving || !deployment.readable || deployment.needsRefresh || !deployment.server || deployment.server.state !== "missing" || deployment.server.read_only || !deployment.approved || !deployment.archivePath.trim();
      return;
    }
    const id = current.openID, spec = deploymentDescriptor(id), deployment = spec && current[spec.state];
    button.hidden = !spec || !deploymentSelected(current, id);
    button.textContent = deployment?.saving ? "正在保存…" : "保存部署";
    button.disabled = !deployment || deployment.loading || deployment.saving || !deployment.server || deployment.server.read_only || !deployment.approved;
  }
  async function loadDeployment(id, current = projectTools) {
    const spec = deploymentDescriptor(id), deployment = spec && current[spec.state];
    if (!deployment || !deploymentSelected(current, id) || deployment.loading || deployment.saving) return;
    const generation = ++deployment.generation;
    deployment.loading = true; deployment.error = "";
    renderProjectToolDialog(current.tools.find((tool) => tool.id === id));
    try {
      const view = await request(`/api/local/project-tools/${id}/deployment`);
      if (!deploymentSelected(current, id) || deployment.generation !== generation) return;
      if (!validDeploymentView(view)) throw new Error("部署登记返回格式无效，请重新读取。");
      deployment.server = view; deployment.loaded = true;
      if (!deployment.draft || !deployment.dirty) deployment.draft = normalizeDeployment(view.configuration, id);
      else deployment.notice = "已读取最新登记，当前编辑已保留；请核对后重新确认保存。";
      deployment.revision = view.revision; deployment.approved = false;
    } catch (error) {
      if (!deploymentSelected(current, id) || deployment.generation !== generation || appSessionExpired([{ status: "rejected", reason: error }])) return;
      deployment.error = error.message || `未能读取 ${spec.name} 部署登记。`;
    } finally {
      if (current === projectTools) {
        deployment.loading = false;
        if (deploymentSelected(current, id)) renderProjectToolDialog(current.tools.find((tool) => tool.id === id));
      }
    }
  }
  function updateDeploymentDraft(id, key, value, current = projectTools) {
    const deployment = current[deploymentDescriptor(id).state];
    if (!deployment.draft) deployment.draft = normalizeDeployment(deployment.server?.configuration, id);
    if (deployment.draft[key] === value) return;
    deployment.draft[key] = value; deployment.dirty = true; deployment.approved = false;
    if (id === "yue" && key === "workflows") delete deployment.draft.workflow_pins;
    if (id === "yue" && key === "ffmpeg") delete deployment.draft.ffmpeg_sha256;
    deployment.error = ""; deployment.notice = "";
    const checkbox = $("#tool-dialog .comfy-deployment-actions input[type=checkbox]");
    if (checkbox) checkbox.checked = false;
    updateDeploymentSave(current);
  }
  async function saveDeployment(id, current = projectTools) {
    const spec = deploymentDescriptor(id), deployment = spec && current[spec.state];
    if (!deployment || !deploymentSelected(current, id) || deployment.loading || deployment.saving || !deployment.server || !deployment.draft || deployment.server.read_only || !deployment.approved) return;
    const checked = validateDeploymentDraft(deployment.draft, id);
    if (typeof checked === "string") {
      deployment.error = checked; renderProjectToolDialog(current.tools.find((tool) => tool.id === id)); return;
    }
    const generation = ++deployment.generation;
    deployment.saving = true; deployment.error = ""; deployment.notice = "";
    renderProjectToolDialog(current.tools.find((tool) => tool.id === id));
    try {
      const view = await request(`/api/local/project-tools/${id}/deployment`, {
        method: "PUT", body: JSON.stringify({ revision: deployment.revision, approve_execution: true, configuration: checked })
      });
      if (!deploymentSelected(current, id) || deployment.generation !== generation) return;
      if (!validDeploymentView(view)) throw new Error("部署登记返回格式无效，请重新读取核对；当前编辑已保留。");
      deployment.server = view; deployment.revision = view.revision; deployment.loaded = true;
      deployment.draft = normalizeDeployment(view.configuration || checked, id); deployment.dirty = false; deployment.approved = false;
      deployment.notice = "部署登记已保存；工具仍需单独启用。";
      void loadProjectTools();
    } catch (error) {
      if (!deploymentSelected(current, id) || deployment.generation !== generation || appSessionExpired([{ status: "rejected", reason: error }])) return;
      deployment.error = error.status === 409
        ? `${error.message || "登记已在别处更新。"} 当前编辑已保留，请重新读取后核对并确认保存。`
        : (error.message || "保存部署登记未完成。");
      if (error.status === 409) deployment.approved = false;
    } finally {
      if (current === projectTools) {
        deployment.saving = false;
        if (deploymentSelected(current, id)) renderProjectToolDialog(current.tools.find((tool) => tool.id === id));
      }
    }
  }
  function comfyDeploymentField(label, key, draft, options = {}) {
    const input = document.createElement(options.select ? "select" : "input");
    input.id = `comfy-deployment-${key}`; input.name = key;
    if (options.select) {
      options.select.forEach(([value, copy]) => {
        const option = document.createElement("option"); option.value = value; option.textContent = copy; input.append(option);
      });
      input.value = String(draft[key]);
    } else {
      input.type = options.type || "text"; input.value = String(draft[key] ?? "");
      if (options.min !== undefined) input.min = String(options.min);
      if (options.max !== undefined) input.max = String(options.max);
      if (options.placeholder) input.placeholder = options.placeholder;
      if (options.inputMode) input.inputMode = options.inputMode;
    }
    input.disabled = Boolean(options.disabled);
    const labelNode = document.createElement("label"); labelNode.htmlFor = input.id; labelNode.textContent = label; labelNode.append(input);
    return { label: labelNode, input };
  }
  function renderDeployment(tool) {
    const current = projectTools, id = tool.id, spec = deploymentDescriptor(id), deployment = current[spec.state];
    const section = appElement("section", "", "comfy-deployment");
    const heading = appElement("div", "", "comfy-deployment-heading");
    heading.append(appElement("strong", "本机部署登记"), appElement("span", id === "yue" ? "登记已准备好的独立 YuE 运行环境。审核清单可留空，由宿主在确认保存时生成。" : "只登记已经准备好的本机目录；不会下载环境或运行安装脚本。审核清单可按本机预览指南生成。"));
    section.append(heading);
    if (deployment.loading && !deployment.loaded) {
      section.append(appElement("p", "正在读取已保存的部署登记…", "comfy-deployment-note"));
      return section;
    }
    if (deployment.error) section.append(appElement("p", deployment.error, "comfy-deployment-error"));
    if (deployment.notice) section.append(appElement("p", deployment.notice, "comfy-deployment-note"));
    const server = deployment.server;
    if (!server) {
      section.append(appElement("p", "部署登记暂时不可读取。关闭后重新打开此工具即可重试。", "comfy-deployment-note"));
      return section;
    }
    const draft = deployment.draft || (deployment.draft = normalizeDeployment(server.configuration, id));
    const readOnly = server.read_only === true, fieldsDisabled = readOnly || deployment.saving || deployment.loading;
    if (readOnly) section.append(appElement("p", server.reason || "请先停用工具并处理未完成任务，再修改部署登记。", "comfy-deployment-readonly"));
    else if (server.reason) section.append(appElement("p", server.reason, "comfy-deployment-note"));
    const form = document.createElement("form"); form.className = "comfy-deployment-form"; form.noValidate = true;
    const primary = appElement("div", "", "comfy-deployment-primary");
    const fields = [
      [`${spec.name} 安装目录`, "runtime", { placeholder: `例如 E:\\Tools\\${spec.name}` }],
      ["模型目录", "models", { placeholder: `例如 E:\\Models\\${spec.name}` }],
      ["工作流目录", "workflows", { placeholder: "包含 registry.json 的目录" }],
      [id === "yue" ? "审核清单（选填，留空由宿主生成）" : "审核清单", "manifest", { placeholder: id === "yue" ? "已有清单可填写；留空自动生成" : "相对安装目录或宿主私有绝对路径" }]
    ];
    if (id === "yue") fields.push(["FFmpeg 程序", "ffmpeg", { placeholder: "已审核的 ffmpeg.exe 绝对路径" }]);
    fields.forEach(([label, key, options]) => {
      const field = comfyDeploymentField(label, key, draft, { ...options, disabled: fieldsDisabled });
      field.input.id = `${spec.prefix}-deployment-${key}`; field.label.htmlFor = field.input.id;
      field.input.addEventListener("input", () => updateDeploymentDraft(id, key, field.input.value, current)); primary.append(field.label);
    });
    form.append(primary);
    const advanced = document.createElement("details"); advanced.className = "comfy-deployment-advanced"; advanced.open = deployment.advancedOpen === true;
    advanced.addEventListener("toggle", () => { deployment.advancedOpen = advanced.open; });
    advanced.append(appElement("summary", "运行细节"));
    const secondary = appElement("div", "", "comfy-deployment-secondary");
    const secondaryFields = [
      ["Python 相对路径", "python", {}], ["入口相对路径", "entry", {}], ["前端相对路径", "frontend", {}],
      ["回环端口", "port", { type: "number", min: 1024, max: 65535, inputMode: "numeric" }],
      ["闲置停止秒数", "idle_seconds", { type: "number", min: 0, max: 9223372036, inputMode: "numeric" }],
      ["计算设备", "device", { select: [["cpu", "CPU"], ["cuda", "CUDA GPU"]] }], ["CUDA 设备编号", "cuda_device", { type: "number", min: 0, max: 64, inputMode: "numeric" }]
    ];
    secondaryFields.forEach(([label, key, options]) => {
      const hidden = key === "cuda_device" && draft.device !== "cuda";
      const field = comfyDeploymentField(label, key, draft, { ...options, disabled: fieldsDisabled });
      field.input.id = `${spec.prefix}-deployment-${key}`; field.label.htmlFor = field.input.id;
      field.label.hidden = hidden;
      field.input.addEventListener("input", () => updateDeploymentDraft(id, key, field.input.value, current));
      field.input.addEventListener("change", () => {
        if (key === "device") { deployment.advancedOpen = true; updateDeploymentDraft(id, key, field.input.value, current); renderProjectToolDialog(tool); }
      });
      secondary.append(field.label);
    });
    advanced.append(secondary, appElement("p", `闲置停止秒数填 0 时，沿用默认 ${spec.idleMinutes} 分钟。`, "comfy-deployment-note")); form.append(advanced);
    const actions = appElement("div", "", "project-tool-actions comfy-deployment-actions");
    const reload = appElement("button", deployment.loading ? "正在读取…" : "重新读取", "button compact secondary");
    reload.type = "button"; reload.disabled = deployment.loading || deployment.saving;
    reload.addEventListener("click", () => void loadDeployment(id, current));
    const approval = document.createElement("label"), checkbox = document.createElement("input"), approvalCopy = appElement("span", "确认这些运行文件和工作流已审核");
    approval.className = "project-tool-approval"; checkbox.type = "checkbox"; checkbox.checked = deployment.approved === true; checkbox.disabled = fieldsDisabled;
    approval.append(checkbox, approvalCopy);
    checkbox.addEventListener("change", () => { deployment.approved = checkbox.checked; updateDeploymentSave(current); });
    form.addEventListener("submit", (event) => { event.preventDefault(); void saveDeployment(id, current); });
    actions.append(reload, approval); form.append(actions); section.append(form);
    section.append(appElement("p", id === "yue" ? "保存仅登记部署；由宿主核验工作流和 FFmpeg 摘要，不启动后端、不启用工具。路径仅供本机主人管理。" : "保存仅登记部署，不启动后端、不启用工具。路径仅供本机主人管理。", "comfy-deployment-note"));
    return section;
  }
  function renderRVCProfiles(tool) {
    const profiles = Array.isArray(tool.voice_profiles) ? tool.voice_profiles.filter((profile) => profile && typeof profile.label === "string" && profile.label.trim()) : [];
    const voices = appElement("div", "", "project-tool-permissions project-tool-voices");
    voices.append(appElement("strong", "可选声线"));
    if (!profiles.length) {
      voices.append(appElement("span", "尚未登记声线。登记完成后，大肥鱼可在聊天任务中选择对应声线。"));
      return voices;
    }
    const list = appElement("div", "", "project-tool-voice-list");
    profiles.forEach((profile) => {
      const label = appElement("span", profile.label.trim(), "project-tool-voice");
      if (typeof profile.id === "string") label.title = profile.id;
      list.append(label);
    });
    voices.append(list);
    return voices;
  }
  function renderComfyWorkflows(tool) {
    const workflows = comfyWorkflows(tool);
    const section = appElement("div", "", "project-tool-permissions project-tool-workflows");
    section.append(appElement("strong", "已登记工作流"));
    if (!workflows.length) {
      section.append(appElement("span", "尚未登记工作流，工具不会把没有工作流的状态显示为可用。"));
      return section;
    }
    section.append(appElement("span", "仅展示工作流名称；启用且本机后端就绪后，才会在获准的图像任务中调用。"));
    const list = appElement("div", "", "project-tool-workflow-list");
    workflows.forEach((workflow) => {
      const item = appElement("span", workflow.label.trim(), "project-tool-workflow");
      item.title = workflow.id.trim();
      list.append(item);
    });
    section.append(list);
    return section;
  }
  function renderYuEBackend(tool) {
    const section = appElement("div", "", "project-tool-permissions project-tool-workflows");
    section.append(appElement("strong", "作曲后端"));
    if (!tool.runtime_ready) {
      section.append(appElement("span", "工具包可以先安装；管理员登记并核验官方 YuE 本机后端后才可启用。"));
      return section;
    }
    section.append(appElement("span", "仅使用宿主登记的官方本机后端。模型、模板和运行环境不会随发行包复制。"));
    return section;
  }
  function renderStationSpeechBackend(tool) {
    const section = appElement("div", "", "project-tool-permissions project-tool-workflows");
    section.append(appElement("strong", "语音后端"));
    if (!tool.runtime_ready) {
      section.append(appElement("span", "工具包可以先安装；管理员登记并核验本机语音后端和声线后才可启用。"));
      return section;
    }
    section.append(appElement("span", "只使用宿主登记的本机语音后端和明确指定的声线；不会用默认声线替代。"));
    return section;
  }
  function renderStationSpeechVoices(tool) {
    const voices = Array.isArray(tool?.speech_voices) ? tool.speech_voices.filter((voice) => voice && typeof voice.id === "string" && voice.id.trim() && typeof voice.label === "string" && voice.label.trim()) : [];
    const section = appElement("div", "", "project-tool-permissions project-tool-voices");
    section.append(appElement("strong", "已登记声线"));
    if (!voices.length) {
      section.append(appElement("span", "尚未登记可用声线。后端登记完成后，聊天任务才能选择明确的声线。"));
      return section;
    }
    const list = appElement("div", "", "project-tool-voice-list");
    voices.forEach((voice) => {
      const label = appElement("span", voice.label.trim(), "project-tool-voice");
      label.title = voice.id.trim();
      list.append(label);
    });
    section.append(list);
    return section;
  }
  function projectToolApprovalCopy(tool) {
    if (tool?.id === "obs-preview") return "确认仅访问本机 OBS WebSocket";
    if (tool?.id === "rvc") return "确认任务素材读写与本机 GPU";
    if (tool?.id === "comfyui") return "确认任务成品写入与本机 GPU";
    if (tool?.id === "yue") return "确认任务素材读写与本机 GPU";
    if (tool?.id === "station-speech") return "确认本机语音成品写入与 GPU";
    return "确认本机素材读写";
  }
  function projectToolPDFNote(tool) {
    if (!tool || !tool.installed) return "安装后会检查 DOCX 导出 PDF 所需的本地环境。";
    if (!tool.runtime_ready) return "本地运行环境未就绪，暂不能导出 PDF。";
    if (tool.pdf_ready === true) return "PDF 导出环境已就绪。";
    if (tool.update_available) return "当前版本尚未准备 PDF 导出环境；停用后更新工具即可检查新版本。";
    return "当前版本尚未确认 PDF 导出环境；DOCX 仍可创建，导出 PDF 前会再次检查。";
  }
  function obsDeploymentSelected(current = projectTools) {
    return activeView === "project-tools" && deploymentSelected(current, "obs-preview");
  }
  function invalidateOBSDeploymentView(current = projectTools) {
    const deployment = current.obsDeployment;
    deployment.generation += 1; deployment.loading = false; deployment.approved = false;
  }
  function validOBSDeploymentView(view) {
    return view && !Array.isArray(view) && typeof view.revision === "string" && view.revision.length > 0
      && view.version === "33.0.1" && view.archive_sha256 === "0d9436451c2d772b33428e3fdae9776f071895c335de17283176306fb72651d3"
      && ["missing", "installed", "invalid"].includes(view.state) && typeof view.installed === "boolean"
      && view.installed === (view.state === "installed") && typeof view.read_only === "boolean" && typeof view.restart_required === "boolean"
      && (view.reason === undefined || typeof view.reason === "string");
  }
  function renderCurrentOBSDeployment(current) {
    if (obsDeploymentSelected(current)) renderProjectToolDialog(current.tools.find((tool) => tool.id === "obs-preview"));
  }
  async function loadOBSDeployment(current = projectTools, explicit = false) {
    const deployment = current.obsDeployment;
    if (!obsDeploymentSelected(current) || deployment.loading || deployment.saving || (deployment.needsRefresh && !explicit)) return;
    const generation = ++deployment.generation;
    deployment.loading = true; deployment.readable = false; deployment.error = "";
    renderCurrentOBSDeployment(current);
    try {
      const view = await request("/api/local/project-tools/obs-preview/deployment");
      if (!obsDeploymentSelected(current) || deployment.generation !== generation) return;
      if (!validOBSDeploymentView(view)) throw new Error("专用输出安装状态响应无效，请刷新后重试。");
      deployment.server = view; deployment.readable = true; deployment.needsRefresh = false; deployment.approved = false; deployment.notice = "";
    } catch (error) {
      if (!obsDeploymentSelected(current) || deployment.generation !== generation || appSessionExpired([{ status: "rejected", reason: error }])) return;
      deployment.error = error.status === 403 ? "当前登录没有专用输出安装管理权限；路径草稿已保留。" : error.message || "无法读取专用输出安装状态。";
    } finally {
      if (current === projectTools && deployment.generation === generation) { deployment.loading = false; renderCurrentOBSDeployment(current); }
    }
  }
  async function installOBSDeployment(current = projectTools) {
    const deployment = current.obsDeployment, server = deployment.server;
    if (!obsDeploymentSelected(current) || deployment.loading || deployment.saving || !deployment.readable || deployment.needsRefresh || !server || server.state !== "missing" || server.read_only || !deployment.approved) return;
    const archivePath = deployment.archivePath.trim();
    if (!/^[A-Za-z]:[\\/].+\.zip$/i.test(archivePath)) {
      deployment.error = "请输入本机 Windows ZIP 文件的绝对路径。"; renderCurrentOBSDeployment(current); return;
    }
    const generation = ++deployment.generation;
    deployment.saving = true; deployment.error = ""; deployment.notice = "";
    renderCurrentOBSDeployment(current);
    try {
      const view = await request("/api/local/project-tools/obs-preview/deployment", { method: "PUT", body: JSON.stringify({ revision: server.revision, archive_path: archivePath, approved: true }) });
      if (current !== projectTools) return;
      if (!obsDeploymentSelected(current) || deployment.generation !== generation) { deployment.needsRefresh = true; return; }
      if (!validOBSDeploymentView(view) || !view.installed) { deployment.needsRefresh = true; throw new Error("安装结果尚未确认，请刷新专用输出状态；路径草稿已保留。"); }
      deployment.server = view; deployment.readable = true; deployment.approved = false;
      deployment.notice = "专用输出已安装。";
    } catch (error) {
      if (current !== projectTools) return;
      if (!obsDeploymentSelected(current) || deployment.generation !== generation) { deployment.needsRefresh = true; return; }
      if (appSessionExpired([{ status: "rejected", reason: error }])) return;
      deployment.approved = false;
      if (error.status === 409) { deployment.readable = false; deployment.needsRefresh = true; }
      deployment.error = error.status === 409 ? `${error.message || "安装状态已变化。"} 请刷新后重新确认；路径草稿已保留。` : error.status === 403 ? "当前登录没有专用输出安装管理权限；路径草稿已保留。" : error.message || "专用输出安装失败，路径草稿已保留。";
    } finally {
      if (current === projectTools) { deployment.saving = false; renderCurrentOBSDeployment(current); }
    }
  }
  function renderOBSDeployment() {
    const current = projectTools, deployment = current.obsDeployment, server = deployment.server;
    const section = appElement("section", "", "comfy-deployment obs-output-deployment"); section.id = "obs-output-deployment";
    const heading = appElement("div", "", "comfy-deployment-heading");
    heading.append(appElement("strong", "专用节目输出"), appElement("span", "为本应用准备独立 OBS 运行环境；下方只读预览连接的是你已有的 OBS，两者用途独立。")); section.append(heading);
    if (deployment.error) section.append(appElement("p", deployment.error, "comfy-deployment-error"));
    if (deployment.notice) section.append(appElement("p", deployment.notice, "comfy-deployment-note"));
    if (deployment.loading) section.append(appElement("p", "正在核验本机安装状态…", "comfy-deployment-note"));
    if (deployment.needsRefresh) section.append(appElement("p", "安装操作已返回，请使用顶栏“刷新”核对最新状态；当前路径草稿已保留。", "comfy-deployment-note"));
    if (!server) { section.append(appElement("p", "等待主人读取安装状态。读取失败时可用顶栏“刷新”重试。", "comfy-deployment-note")); return section; }
    if (server.installed) {
      section.append(appElement("p", server.restart_required ? "OBS 33.0.1 已安装 · 重启应用后启用专用输出。" : "OBS 33.0.1 已安装 · 专用输出环境已就绪。", "comfy-deployment-note"));
      return section;
    }
    const version = appElement("p", "仅接受官方 Windows x64 ZIP · OBS 33.0.1，由宿主核验固定 SHA-256。", "comfy-deployment-note");
    const release = appElement("a", "查看官方下载页"); release.href = "https://github.com/obsproject/obs-studio/releases/tag/33.0.1"; release.target = "_blank"; release.rel = "noopener noreferrer";
    version.append(document.createTextNode(" "), release); section.append(version);
    if (server.reason) section.append(appElement("p", server.reason, server.read_only ? "comfy-deployment-readonly" : "comfy-deployment-note"));
    if (server.state === "invalid") section.append(appElement("p", "已检测到的专用环境未通过核验，请按当前状态处理后重试。", "comfy-deployment-note"));
    const form = document.createElement("form"); form.className = "comfy-deployment-form"; form.noValidate = true;
    const label = appElement("label", "本机官方 ZIP 绝对路径"), input = document.createElement("input");
    input.type = "text"; input.id = "obs-output-archive-path"; input.name = "archive_path"; input.value = deployment.archivePath; input.placeholder = "C:\\Downloads\\OBS-Studio-33.0.1-Windows-x64.zip"; input.autocomplete = "off"; input.spellcheck = false;
    input.disabled = deployment.loading || deployment.saving || server.read_only; label.htmlFor = input.id; label.append(input);
    input.addEventListener("input", () => { if (deployment.saving) return; deployment.archivePath = input.value; deployment.approved = false; const approval = $("#obs-output-approved"); if (approval) approval.checked = false; updateDeploymentSave(current); });
    const approval = appElement("label", "", "project-tool-approval"), checkbox = document.createElement("input");
    checkbox.type = "checkbox"; checkbox.id = "obs-output-approved"; checkbox.checked = deployment.approved; checkbox.disabled = input.disabled || !deployment.readable || deployment.needsRefresh || server.state !== "missing";
    checkbox.addEventListener("change", () => { deployment.approved = checkbox.checked; updateDeploymentSave(current); });
    approval.append(checkbox, document.createTextNode("确认从此本机 ZIP 安装专用输出；安装不授予直播权限。"));
    form.append(label, approval); form.addEventListener("submit", (event) => { event.preventDefault(); void installOBSDeployment(current); }); section.append(form);
    section.append(appElement("p", "安装只校验并解压此 ZIP；不会联网下载、运行脚本、启用工具、重启应用或开播。", "comfy-deployment-note"));
    return section;
  }
  function obsPreviewTool() { return projectTools.tools.find((tool) => tool && tool.id === "obs-preview") || null; }
  function decodeOBSPreviewImage(value) {
    if (typeof value !== "string" || !/^data:image\/jpeg;base64,[A-Za-z0-9+/]+={0,2}$/.test(value)) return null;
    try {
      const raw = atob(value.slice("data:image/jpeg;base64,".length));
      const bytes = Uint8Array.from(raw, (character) => character.charCodeAt(0));
      return bytes.length >= 4 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes.at(-2) === 0xff && bytes.at(-1) === 0xd9 ? bytes : null;
    } catch (_) { return null; }
  }
  function replaceOBSPreviewImage(value) {
    const preview = projectTools.obsPreview;
    if (preview.imageURL) URL.revokeObjectURL(preview.imageURL);
    preview.imageURL = "";
    const bytes = decodeOBSPreviewImage(value);
    if (bytes) preview.imageURL = URL.createObjectURL(new Blob([bytes], { type: "image/jpeg" }));
  }
  function clearOBSPreview() {
    const preview = projectTools.obsPreview;
    preview.generation += 1; window.clearTimeout(preview.timer); preview.timer = 0;
    preview.active = false; preview.loading = false; preview.applying = false; preview.polling = false; preview.starting = false; preview.stopping = false;
    replaceOBSPreviewImage(""); preview.scene = ""; preview.observedAt = "";
  }
  function applyOBSPreview(payload) {
    const preview = projectTools.obsPreview;
    if (!payload || typeof payload !== "object") return;
    preview.configured = payload.configured === true;
    preview.port = Number.isInteger(Number(payload.port)) && Number(payload.port) > 0 && Number(payload.port) <= 65535 ? Number(payload.port) : 4455;
    preview.configRevision = Number.isFinite(Number(payload.config_revision)) ? Number(payload.config_revision) : 0;
    preview.enabled = payload.enabled === true;
    preview.active = payload.active === true;
    preview.scene = typeof payload.scene === "string" ? payload.scene.slice(0, 180) : "";
    preview.observedAt = typeof payload.observed_at === "string" ? payload.observed_at.slice(0, 80) : "";
    if (preview.active && payload.image_data) {
      replaceOBSPreviewImage(payload.image_data);
    } else if (!preview.active) {
      window.clearTimeout(preview.timer); preview.timer = 0; preview.polling = false;
      replaceOBSPreviewImage(""); preview.scene = ""; preview.observedAt = "";
    }
  }
  function scheduleOBSPreviewFrame() {
    const preview = projectTools.obsPreview;
    window.clearTimeout(preview.timer);
    if (activeView !== "project-tools" || !preview.active || !preview.enabled || preview.polling || preview.stopping) return;
    preview.timer = window.setTimeout(() => void requestOBSPreview("frame", true), 1500);
  }
  async function loadOBSPreview(current = projectTools) {
    const tool = current.tools.find((item) => item && item.id === "obs-preview");
    if (!tool || !tool.installed) { clearOBSPreview(); return; }
    const preview = current.obsPreview;
    if (preview.loading) return;
    preview.loading = true;
    const generation = preview.generation;
    try {
      const value = await request("/api/local/project-tools/obs-preview/preview");
      if (current !== projectTools || generation !== preview.generation) return;
      applyOBSPreview(value);
    } catch (error) {
      if (current !== projectTools || generation !== preview.generation) return;
      clearOBSPreview();
      if (error.status === 401 || error.status === 403) { state.csrf = ""; showLogin(); return; }
      current.obsPreview.error = error.message || "无法读取 OBS 预览状态。";
    } finally {
      if (current === projectTools && generation === preview.generation) { preview.loading = false; renderProjectTools(); scheduleOBSPreviewFrame(); }
    }
  }
  function updateOBSPreviewPanel() {
    const panel = $("#obs-preview-panel"), tool = obsPreviewTool();
    if (panel && tool) panel.replaceWith(renderOBSPreview(tool));
  }
  async function requestOBSPreview(action, background = false, form = null) {
    const current = projectTools, tool = obsPreviewTool(), preview = current.obsPreview;
    if (!tool || !tool.installed || ((preview.polling || preview.applying) && action !== "stop")) return;
    if (action === "frame" && (!preview.active || activeView !== "project-tools")) return;
    if (action === "start") { preview.generation += 1; preview.starting = true; }
    if (action === "stop") { preview.generation += 1; window.clearTimeout(preview.timer); preview.timer = 0; preview.stopping = true; }
    const generation = preview.generation;
    preview.polling = action === "frame";
    preview.applying = action !== "frame";
    preview.error = "";
    if (!background) renderProjectTools();
    try {
      const body = { action };
      if (action === "configure") {
        const port = Number(form?.port?.value || 0);
        const password = String(form?.password?.value || "");
        if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error("请输入 1 到 65535 之间的本机回环端口。");
        if (!password) throw new Error("请填写 OBS WebSocket 密码；保存后不会再次显示。");
        body.port = port; body.password = password; body.config_revision = preview.configRevision;
      }
      const value = await request("/api/local/project-tools/obs-preview/preview", { method: "POST", body: JSON.stringify(body) });
      if (current !== projectTools || generation !== preview.generation) return;
      applyOBSPreview(value);
      if (action === "configure" && form) form.password.value = "";
      if (action === "stop") projectToolsFeedback("已停止 OBS 预览。画面不会保存在浏览器中。");
      if (action === "start") projectToolsFeedback("正在显示 OBS 当前节目画面。");
    } catch (error) {
      if (current !== projectTools || generation !== preview.generation) return;
      if (error.status === 401 || error.status === 403) { clearOBSPreview(); state.csrf = ""; showLogin(); return; }
      preview.error = error.message || "OBS 预览操作未完成。";
      if (action === "stop" || action === "frame") { clearOBSPreview(); updateOBSPreviewPanel(); }
    } finally {
      if (current === projectTools && generation === preview.generation) {
        preview.polling = false; preview.starting = false; if (action !== "frame") preview.applying = false;
        if (action === "frame") updateOBSPreviewPanel(); else renderProjectTools();
        scheduleOBSPreviewFrame();
      }
    }
  }
  function stopOBSPreviewOnLeave() {
    const preview = projectTools.obsPreview;
    const shouldStop = preview.active || preview.starting;
    clearOBSPreview();
    if (!shouldStop || !state.csrf) return;
    request("/api/local/project-tools/obs-preview/preview", { method: "POST", body: JSON.stringify({ action: "stop" }) }).catch(() => {});
  }
  function renderOBSPreview(tool) {
    const preview = projectTools.obsPreview;
    const panel = appElement("section", "", "obs-preview-panel"); panel.id = "obs-preview-panel";
    panel.append(appElement("strong", "OBS 当前节目画面"));
    const note = !tool.installed ? "安装后可登记本机 OBS WebSocket，再按需查看节目画面。"
      : !tool.enabled ? "先保存本机 OBS 凭据，再启用工具；停用时不会保留浏览器中的画面。"
      : !preview.configured ? "还没有保存 OBS 凭据。请先停用工具并登记本机回环端口与密码。"
      : preview.active ? "预览只读取 OBS 当前节目画面，离开本页、停用工具或 60 秒无操作后会自动清除。"
      : "已准备好。开始预览后只显示当前节目画面，不会开播、切换场景或写入 OBS。";
    panel.append(appElement("span", note, "obs-preview-note"));
    if (preview.error) panel.append(appElement("p", preview.error, "obs-preview-error"));
    if (tool.installed && !tool.enabled) {
      const form = document.createElement("form"); form.className = "obs-preview-config";
      form.noValidate = true;
      const port = document.createElement("input"); port.id = "obs-preview-port"; port.name = "port"; port.type = "number"; port.min = "1"; port.max = "65535"; port.value = String(preview.port || 4455); port.required = true;
      const password = document.createElement("input"); password.id = "obs-preview-password"; password.name = "password"; password.type = "password"; password.autocomplete = "new-password"; password.required = true;
      const portLabel = document.createElement("label"); portLabel.htmlFor = port.id; portLabel.textContent = "OBS 回环端口"; portLabel.append(port);
      const passwordLabel = document.createElement("label"); passwordLabel.htmlFor = password.id; passwordLabel.textContent = "OBS WebSocket 密码"; passwordLabel.append(password);
      const save = appElement("button", preview.applying ? "正在保存…" : "保存凭据", "button compact secondary"); save.type = "submit"; save.disabled = preview.applying;
      form.append(portLabel, passwordLabel, save);
      form.addEventListener("submit", (event) => { event.preventDefault(); void requestOBSPreview("configure", false, form); });
      panel.append(form);
    }
    if (tool.installed && tool.enabled) {
      const controls = appElement("div", "", "project-tool-actions");
      const start = appElement("button", preview.applying ? "正在启动…" : "开始预览", "button compact primary"); start.type = "button"; start.disabled = preview.applying || preview.stopping || !preview.configured || preview.active;
      start.addEventListener("click", () => void requestOBSPreview("start"));
      const stop = appElement("button", preview.stopping ? "正在停止…" : "停止预览", "button compact secondary"); stop.type = "button"; stop.disabled = preview.stopping || !preview.active;
      stop.addEventListener("click", () => void requestOBSPreview("stop"));
      controls.append(preview.active || preview.starting || preview.stopping ? stop : start);
      panel.append(controls);
    }
    if (preview.active && preview.imageURL) {
      const image = document.createElement("img"); image.className = "obs-preview-image"; image.alt = preview.scene ? `OBS 节目画面：${preview.scene}` : "OBS 当前节目画面"; image.src = preview.imageURL;
      panel.append(image);
      const timestamp = new Date(preview.observedAt);
      const updated = Number.isFinite(timestamp.getTime()) ? timestamp.toLocaleString("zh-CN", { hour12: false }) : "";
      const metadata = [preview.scene ? `场景：${preview.scene}` : "当前节目画面", updated ? `更新于 ${updated}` : "正在读取"].join(" · ");
      panel.append(appElement("span", metadata, "obs-preview-meta"));
    }
    return panel;
  }
  function renderProjectTools() {
    const list = $("#project-tools-list"), current = projectTools;
    list.setAttribute("aria-busy", String(current.loading || Boolean(current.applying)));
    $("#project-tools-refresh").disabled = current.loading || Boolean(current.applying);
    $("#project-tools-refresh").textContent = current.loading ? "刷新中…" : "刷新";
    if (current.loading && !current.tools.length) {
      list.replaceChildren(appElement("p", "正在读取项目工具…", "project-tools-empty"));
      return;
    }
    if (!current.tools.length) {
      list.replaceChildren(appElement("p", "当前没有可管理的项目工具。", "project-tools-empty"));
      return;
    }
    list.replaceChildren(...current.tools.map((tool) => {
      const [status] = projectToolStatus(tool);
      const card = appElement("button", "", "project-tool-card");
      card.type = "button"; card.dataset.tool = tool.id;
      card.setAttribute("aria-label", `管理${tool.name || tool.id}`);
      const heading = appElement("div", "", "tool-card-heading");
      const mark = appElement("span", (tool.name || "工").slice(0, 1), "project-tool-mark");
      mark.setAttribute("aria-hidden", "true");
      heading.append(mark, appElement("strong", tool.name || tool.id));
      card.append(heading, appElement("span", status, "tool-card-status"), appElement("span", "设置与资源", "tool-card-link"));
      card.addEventListener("click", () => {
        if (current.openID === "obs-preview") invalidateOBSDeploymentView(current);
        current.openID = tool.id; renderProjectToolDialog(tool);
        $("#tool-dialog").showModal();
        updateDeploymentSave(current);
        if (deploymentDescriptor(tool.id)) void loadDeployment(tool.id, current);
        if (tool.id === "obs-preview") void loadOBSDeployment(current);
        if (tool.id === "media-understanding" && tool.installed) void loadLocalVision(current);
      });
      return card;
    }));
    const selected = current.tools.find((tool) => tool.id === current.openID);
    if (selected && $("#tool-dialog").open) renderProjectToolDialog(selected);
  }
  function renderProjectToolDialog(tool) {
    const current = projectTools, body = $("#tool-dialog-body"), scroll = body.scrollTop;
    $("#tool-dialog-title").textContent = tool.name || tool.id;
    let visionSave = $("#local-vision-save");
    if (!visionSave) {
      visionSave = appElement("button", "保存视觉设置", "button compact primary"); visionSave.id = "local-vision-save"; visionSave.type = "button";
      visionSave.addEventListener("click", () => { void saveLocalVision(); });
      $("#tool-dialog .app-dialog-actions").append(visionSave);
    }
    updateLocalVisionSave(current);
    let deploymentSave = $("#local-deployment-save");
    if (!deploymentSave) {
      deploymentSave = appElement("button", "保存部署", "button compact primary"); deploymentSave.id = "local-deployment-save"; deploymentSave.type = "button";
      deploymentSave.addEventListener("click", () => { if (projectTools.openID === "obs-preview") void installOBSDeployment(); else if (deploymentDescriptor(projectTools.openID)) void saveDeployment(projectTools.openID); });
      $("#tool-dialog .app-dialog-actions").append(deploymentSave);
    }
    updateDeploymentSave(current);
    $("#tool-dialog-feedback").append($("#project-tools-feedback"));
    body.replaceChildren(projectToolDetails(tool, current));
    body.scrollTop = scroll;
    $("#tool-dialog-refresh").disabled = current.loading || Boolean(current.applying);
    $("#tool-dialog-refresh").textContent = current.loading ? "刷新中…" : "刷新";
  }
  function projectToolDetails(tool, current) {
      const card = appElement("article", "", "project-tool-record");
      const [, note] = projectToolStatus(tool);
      const detail = appElement("div", "", "project-tool-detail");
      if (note && tool.id !== "obs-preview") detail.append(appElement("p", note));
      if (tool.id === "media-understanding" && tool.installed) detail.append(renderLocalVision(tool));
      if (tool.id === "audio-workshop") {
        const scope = appElement("div", "", "project-tool-permissions");
        scope.append(appElement("strong", "音频处理"), appElement("span", "截取本机 WAV 成品，可转单声道，输出 16 位 WAV。每次仅访问获准的输入和任务输出。"));
        detail.append(scope);
      }
      if (tool.id === "rvc") {
        const permissions = appElement("div", "", "project-tool-permissions");
        permissions.append(appElement("strong", "换声任务"), appElement("span", "使用本次获准的音频和指定声线，可调用本机 GPU。通过核验的音频可从聊天下载。"));
        detail.append(permissions, renderRVCProfiles(tool));
      }
      if (tool.id === "comfyui") {
        const permissions = appElement("div", "", "project-tool-permissions");
        permissions.append(appElement("strong", "图像任务"), appElement("span", "运行获准的工作流，使用本机计算资源并登记 PNG 成品。"));
        detail.append(permissions, renderDeployment(tool), renderComfyWorkflows(tool));
      }
      if (tool.id === "yue") {
        const permissions = appElement("div", "", "project-tool-permissions");
        permissions.append(appElement("strong", "作曲任务"), appElement("span", "仅读取本次获准的参考素材，使用本机 GPU，核验后登记音乐成品。不会自动发送。"));
        detail.append(permissions, renderDeployment(tool), renderYuEBackend(tool));
      }
      if (tool.id === "station-speech") {
        const permissions = appElement("div", "", "project-tool-permissions");
        permissions.append(appElement("strong", "语音合成"), appElement("span", "用明确选择的本机声线合成文本，核验后登记音频成品。不会自动发送。"));
        detail.append(permissions, renderStationSpeechBackend(tool), renderStationSpeechVoices(tool));
      }
      if (tool.id === "document-workshop") {
        const permissions = appElement("div", "", "project-tool-permissions");
        permissions.append(appElement("strong", "本次授权"), appElement("span", "本机素材读写限于授权的文档任务，成品保存在本机，可从聊天下载。联网和应用账号权限另行管理。"));
        const pdf = appElement("div", "", "project-tool-permissions");
        pdf.append(appElement("strong", "PDF 导出"), appElement("span", projectToolPDFNote(tool)));
        detail.append(permissions);
        detail.append(pdf);
      }
      if (tool.id === "obs-preview") detail.append(renderOBSDeployment(), renderOBSPreview(tool));
      const controls = appElement("div", "", "project-tool-actions");
      const busy = current.applying === tool.id;
      const controlsDisabled = current.loading || Boolean(current.applying);
      const canInstall = tool.runtime_ready || tool.id === "comfyui" || tool.id === "yue" || tool.id === "station-speech" || tool.id === "obs-preview";
      if (!tool.installed) {
        const approval = document.createElement("label"), checkbox = document.createElement("input"), copy = appElement("span", projectToolApprovalCopy(tool));
        approval.className = "project-tool-approval"; checkbox.type = "checkbox"; checkbox.disabled = controlsDisabled || !tool.catalog_available || !canInstall;
        approval.append(checkbox, copy);
        const install = appElement("button", busy ? "正在安装…" : tool.id === "obs-preview" ? "安装预览工具" : "安装", "button compact primary");
        install.type = "button"; install.disabled = true;
        checkbox.addEventListener("change", () => { install.disabled = checkbox.disabled || !checkbox.checked; });
        install.addEventListener("click", () => updateProjectTool(tool, "install", checkbox.checked));
        controls.append(approval, install);
      } else {
        const action = tool.enabled ? "disable" : "enable";
        const button = appElement("button", busy ? "正在处理…" : (tool.enabled ? "停用" : "启用"), tool.enabled ? "button compact secondary" : "button compact primary");
        const ready = tool.id === "obs-preview" ? current.obsPreview.configured && !current.obsPreview.loading : tool.runtime_ready;
        button.type = "button"; button.disabled = controlsDisabled || (action === "enable" && !ready);
        button.addEventListener("click", () => updateProjectTool(tool, action, false));
        controls.append(button);
        if (tool.update_available) {
          if (tool.enabled) {
            controls.append(appElement("span", "请先停用，再更新到新版本。", "project-tool-update-note"));
          } else {
            const approval = document.createElement("label"), checkbox = document.createElement("input"), copy = appElement("span", projectToolApprovalCopy(tool));
            approval.className = "project-tool-approval"; checkbox.type = "checkbox"; checkbox.disabled = controlsDisabled || !canInstall;
            approval.append(checkbox, copy);
            const upgrade = appElement("button", busy ? "正在更新…" : "更新", "button compact secondary");
            upgrade.type = "button"; upgrade.disabled = true;
            checkbox.addEventListener("change", () => { upgrade.disabled = checkbox.disabled || !checkbox.checked; });
            upgrade.addEventListener("click", () => updateProjectTool(tool, "upgrade", checkbox.checked));
            controls.append(approval, upgrade);
          }
        }
      }
      detail.append(controls); card.append(detail); return card;
  }

  async function loadProjectTools() {
    const current = projectTools;
    if (current.loading || !state.home?.initialized) return;
    current.loading = true; renderProjectTools();
    try {
      const payload = await request("/api/local/project-tools");
      if (current !== projectTools) return;
      current.tools = Array.isArray(payload.tools) ? payload.tools.filter((tool) => tool && typeof tool.id === "string") : [];
      const obs = current.tools.find((tool) => tool.id === "obs-preview");
      if (obs?.obs_preview) applyOBSPreview(obs.obs_preview);
      if (obs?.installed) await loadOBSPreview(current);
      else clearOBSPreview();
    } catch (error) {
      if (current === projectTools) projectToolsFeedback(error.message, true);
    } finally {
      if (current === projectTools) { current.loading = false; renderProjectTools(); }
    }
  }
  async function updateProjectTool(tool, action, approveArtifacts) {
    const current = projectTools;
    if (current.loading || current.applying || !tool || !/^[a-z0-9-]+$/.test(tool.id)) return;
    current.applying = tool.id; projectToolsFeedback(""); renderProjectTools();
    try {
      const result = await request("/api/local/project-tools/" + encodeURIComponent(tool.id), { method: "PUT", body: JSON.stringify({ action, revision: Number(tool.revision || 0), approve_artifacts: Boolean(approveArtifacts) }) });
      if (current !== projectTools) return;
      const updated = result && result.tool;
      if (updated && typeof updated.id === "string") current.tools = current.tools.map((item) => item.id === updated.id ? updated : item);
      if (tool.id === "obs-preview") { current.obsPreview.enabled = updated?.enabled === true; if (action === "disable") clearOBSPreview(); }
      if (tool.id === "obs-preview" && (action === "install" || action === "upgrade")) await loadOBSPreview(current);
      const statuses = { install: "已安装", enable: "已启用", upgrade: "已更新", disable: "已停用" };
      projectToolsFeedback((tool.name || tool.id) + statuses[action] + "。");
    } catch (error) {
      if (current === projectTools) projectToolsFeedback(error.message, true);
    } finally {
      if (current === projectTools) { current.applying = ""; renderProjectTools(); }
    }
  }
  function showLogin() {
    stopDouyinLiveInspection(apps);
    $("#tool-dialog").close();
    $("#app-dialog").close();
    state.home = null; state.model = null; window.clearTimeout(state.chat.poll); state.chat = newChatView(); renderChat();
    window.clearTimeout(apps.loginTimer);
    stopDouyinWebLoginPolling(apps);
    clearOBSPreview();
    apps = newAppView();
    projectTools = newProjectToolsView();
    $("#apps-list").replaceChildren();
    appFeedback("");
    $("#project-tools-list").replaceChildren();
    projectToolsFeedback("");
    setActiveView("chat");
    document.body.classList.remove("is-home-ready");
    localWorkspace.hidden = true; homeView.hidden = true; loginView.hidden = false; $("#login-password").value = "";
  }
  function shortId(id, fallback) { const value = String(id || ""); return value ? `${value.slice(0, 11)}…${value.slice(-6)}` : fallback; }
  function showHome(home) {
    state.home = home; loginView.hidden = true; homeView.hidden = false;
    const initialized = Boolean(home.initialized), enabled = Boolean(home.home_access), chatAvailable = home.chat_available !== false;
    $("#status-kicker").textContent = initialized ? "本地相处" : "准备中";
    $("#status-title").textContent = initialized ? "大肥鱼在家" : "为大肥鱼准备家";
    $("#stage-note").textContent = !initialized
      ? "准备家目录后，就能和她聊天，并让她整理自己的文件与工具。"
      : chatAvailable
        ? enabled
          ? "她可以整理家目录中的文件、项目与工具；程序执行需要已配置的 Windows 运行器。"
          : "聊天始终可用；工作空间已暂停，文件操作与新程序启动已停止。"
        : "本地聊天请从独立 Agent-home-station 服务进入。";
    initializeButton.hidden = initialized; $("#setup-actions").hidden = initialized && chatAvailable; localWorkspace.hidden = !initialized || !chatAvailable;
    document.body.classList.toggle("is-home-ready", initialized && chatAvailable);
    for (const button of [accessButton, settingsAccessButton]) {
      button.hidden = !initialized && button === accessButton;
      button.textContent = enabled ? "暂停工作空间授权" : "恢复工作空间授权";
      button.className = `button ${enabled ? "secondary" : "primary"}${button === settingsAccessButton ? " compact" : ""}`;
    }
    const chip = $("#access-chip");
    chip.textContent = !initialized ? "尚未准备" : enabled ? "工作空间已授权" : "工作空间已暂停";
    chip.classList.toggle("is-live", enabled);
    $("#home-directory").textContent = home.directory || "专属目录正在准备";
    $("#fish-id").textContent = shortId(home.fish_id, "大肥鱼"); $("#fish-id").title = home.fish_id || "大肥鱼";
    $("#owner-id").textContent = shortId(home.owner_id, "本机主人"); $("#owner-id").title = home.owner_id || "本机主人";
  }
  async function loadHome() {
    const home = await request("/api/local/home");
    showHome(home);
    if (home.initialized && home.chat_available !== false) {
      await Promise.all([loadModel(state.model === null), loadChat(true), activeView === "apps" ? loadApps() : undefined, activeView === "project-tools" ? loadProjectTools() : undefined]);
    }
  }
  function setModelForm(model) {
    const source = model || {};
    $("#model-revision").value = String(source.revision || 0);
    $("#model-provider").value = source.provider || "deepseek";
    $("#model-base-url").value = source.base_url || "https://api.deepseek.com";
    $("#model-name").value = source.model || "deepseek-flash";
    $("#model-max-tokens").value = String(source.max_tokens || 4096);
    $("#model-thinking").checked = source.thinking_enabled !== false;
    $("#model-reasoning").value = source.reasoning_effort || "low";
    $("#model-persona").value = source.persona || "";
    $("#model-api-key").value = ""; $("#model-api-key").placeholder = source.key_configured ? "已保存；留空不改" : "填写后保存";
    $("#model-clear-key").checked = false;
  }
  async function loadModel(syncForm = false) {
    try {
      state.model = await request("/api/local/model");
      if (syncForm) setModelForm(state.model);
      $("#model-setup-note").hidden = chatConfigured(); $("#open-settings-button").hidden = chatConfigured();
    } catch (error) {
      state.model = null; $("#model-setup-note").hidden = false; $("#open-settings-button").hidden = false;
      if (error.status !== 404) showMessage(error.message, "warn");
    }
  }
  function messageTime(value) {
    if (!value) return "";
    const date = new Date(value); if (Number.isNaN(date.getTime())) return String(value).slice(0, 16);
    return new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit" }).format(date);
  }
  function itemText(item) { return typeof item.text === "string" ? item.text : (typeof item.content === "string" ? item.content : ""); }
  function renderFailedDraft(requestID, failure) {
    const bubble = document.createElement("article"), meta = document.createElement("div"), body = document.createElement("p"), line = document.createElement("div"), detail = document.createElement("span"), retry = document.createElement("button");
    bubble.className = "chat-bubble is-user";
    meta.className = "bubble-meta"; meta.textContent = "你";
    body.className = "bubble-text"; body.textContent = failure.text;
    line.className = "bubble-failure"; detail.textContent = failure.message;
    retry.type = "button"; retry.className = "text-action"; retry.textContent = "重试";
    retry.addEventListener("click", () => sendMessage(failure.text, requestID));
    line.append(detail, retry); bubble.append(meta, body, line);
    chatStream.append(bubble);
  }
  function renderPendingDraft(failure) {
    const bubble = document.createElement("article"), meta = document.createElement("div"), body = document.createElement("p");
    bubble.className = "chat-bubble is-user is-pending";
    meta.className = "bubble-meta"; meta.textContent = "你 · 正在提交";
    body.className = "bubble-text"; body.textContent = failure.text;
    bubble.append(meta, body); chatStream.append(bubble);
  }
  function appendArtifactDownloads(bubble, text) {
    const identifiers = new Set(String(text || "").match(/\bwork-[a-f0-9]{64}\b/g) || []);
    if (!identifiers.size) return;
    const downloads = appElement("div", "", "artifact-downloads");
    for (const identifier of identifiers) {
      const link = document.createElement("a");
      link.className = "artifact-download";
      link.textContent = "下载已登记成品";
      link.href = "/api/local/artifacts/" + encodeURIComponent(identifier) + "/download";
      link.setAttribute("download", "");
      downloads.append(link);
    }
    bubble.append(downloads);
  }
  function renderChat() {
    const previousScroll = chatStream.scrollTop;
    chatStream.replaceChildren();
    if (!state.chat.items.length) {
      const empty = document.createElement("div"), title = document.createElement("strong"), hint = document.createElement("span");
      empty.className = "chat-empty"; title.textContent = "从一句话开始"; hint.textContent = "大肥鱼会在这里记下这段对话。";
      empty.append(title, hint);
      chatStream.append(empty);
    }
    const latestAssistant = state.chat.items.reduce((latest, item, index) => item.role === "assistant" ? index : latest, -1);
    let recoveryID = "";
    if (!state.chat.busy && !state.chat.pending.size && !state.chat.backgroundPending) {
      for (let index = state.chat.items.length - 1; index > latestAssistant; index -= 1) {
        const item = state.chat.items[index];
        if (item.role !== "assistant" && item.request_id) { recoveryID = item.request_id; break; }
      }
    }
    for (const item of state.chat.items) {
      const assistant = item.role === "assistant", bubble = document.createElement("article");
      bubble.className = `chat-bubble is-${assistant ? "assistant" : "user"}`;
      if (!assistant && state.chat.pending.has(item.request_id)) bubble.classList.add("is-pending");
      const meta = document.createElement("div"); meta.className = "bubble-meta"; meta.textContent = assistant ? "大肥鱼" : "你";
      const time = messageTime(item.time || item.created_at);
      if (time) { const timeNode = document.createElement("time"); timeNode.textContent = time; meta.append(timeNode); }
      const text = itemText(item), body = document.createElement("p"); body.className = "bubble-text"; body.textContent = text; bubble.append(meta, body);
      if (assistant) appendArtifactDownloads(bubble, text);
      if (!assistant && (state.chat.failed.has(item.request_id) || item.request_id === recoveryID)) {
        const failure = state.chat.failed.get(item.request_id) || { message: "未收到回复。" }, line = document.createElement("div"), detail = document.createElement("span"), retry = document.createElement("button");
        line.className = "bubble-failure"; detail.textContent = failure.message; retry.type = "button"; retry.className = "text-action"; retry.textContent = "重试";
        retry.addEventListener("click", () => sendMessage(itemText(item), item.request_id)); line.append(detail, retry); bubble.append(line);
      }
      chatStream.append(bubble);
    }
    const persistedRequestIDs = new Set(state.chat.items.filter((item) => item.role !== "assistant").map((item) => item.request_id));
    for (const [requestID, pending] of state.chat.pending) {
      if (!persistedRequestIDs.has(requestID)) renderPendingDraft(pending);
    }
    for (const [requestID, failure] of state.chat.failed) {
      if (!persistedRequestIDs.has(requestID)) renderFailedDraft(requestID, failure);
    }
    $("#load-older-button").hidden = !state.chat.hasMore || state.chat.loading;
    $("#chat-progress").textContent = state.chat.busy || state.chat.pending.size ? "正在等待回复…" : state.chat.backgroundPending ? "后台任务完成后会继续回复" : "";
    chatStream.setAttribute("aria-busy", String(state.chat.loading || state.chat.busy || state.chat.pending.size > 0));
    chatStream.scrollTop = previousScroll;
  }
  function mergeItems(incoming, replace) {
    const mapped = replace ? new Map() : new Map(state.chat.items.map((item) => [item.id || `${item.role}/${item.time}/${item.text}`, item]));
    for (const item of incoming) mapped.set(item.id || `${item.role}/${item.time}/${item.text}`, item);
    state.chat.items = Array.from(mapped.values()).sort((a, b) => String(a.time || a.created_at || "").localeCompare(String(b.time || b.created_at || "")));
  }
  async function loadChat(replace = false) {
    const current = state.chat;
    if (!state.home || !state.home.initialized) return;
    if (current.loading) { if (replace) current.refreshRequested = true; return; }
    current.loading = true; current.refreshRequested = false; renderChat();
    try {
      const query = new URLSearchParams({ limit: "40" }); if (!replace && current.nextCursor) query.set("cursor", current.nextCursor);
      const data = await request(`/api/local/chat?${query.toString()}`);
      if (current !== state.chat) return;
      mergeItems(Array.isArray(data.items) ? data.items : [], replace); state.chat.nextCursor = data.next_cursor || ""; state.chat.hasMore = Boolean(data.has_more);
      state.chat.busy = Boolean(data.busy);
      current.backgroundPending = data.background_pending === true;
      const settled = new Set(state.chat.items.filter((item) => item.role === "assistant" && item.request_id).map((item) => item.request_id));
      for (const requestID of settled) { state.chat.pending.delete(requestID); state.chat.failed.delete(requestID); }
    } catch (error) {
      if (current !== state.chat || appSessionExpired([{ status: "rejected", reason: error }])) return;
      if (error.status !== 404) showMessage(error.message, "warn");
    } finally {
      if (current === state.chat) {
        current.loading = false; renderChat();
        if (current.refreshRequested && activeView === "chat" && !document.hidden) void loadChat(true);
        else schedulePoll();
      }
    }
  }
  function schedulePoll() {
    const current = state.chat;
    window.clearTimeout(current.poll); current.poll = 0;
    if (!state.home?.initialized || activeView !== "chat" || document.hidden || !(current.busy || current.pending.size || current.backgroundPending)) return;
    current.poll = window.setTimeout(() => { if (current === state.chat) void loadChat(true); }, 1600);
  }
  function newRequestID() { return window.crypto && typeof window.crypto.randomUUID === "function" ? window.crypto.randomUUID() : `local-${Date.now()}-${Math.random().toString(16).slice(2)}`; }
  async function sendMessage(raw, requestID = newRequestID()) {
    const current = state.chat;
    const text = String(raw || ""); if (!text.trim() || state.chat.pending.has(requestID)) return;
    if (!chatConfigured()) { setActiveView("settings"); showMessage("先保存可用的聊天模型，再发送消息。", "warn"); return; }
    state.chat.pending.set(requestID, { text }); state.chat.failed.delete(requestID); $("#chat-input").value = ""; renderChat();
    // Only an explicit send/retry brings its result into view. Background
    // refreshes and reading older pages preserve the user's scroll position.
    chatStream.scrollTop = chatStream.scrollHeight;
    schedulePoll();
    try {
      const result = await request("/api/local/chat", { method: "POST", body: JSON.stringify({ request_id: requestID, text }) });
      if (current !== state.chat) return;
      if (result.status === "completed") state.chat.pending.delete(requestID);
      if (result.status === "no_reply") {
        state.chat.pending.delete(requestID);
        showMessage("本轮没有生成回复。", "warn");
      }
      await loadChat(true);
    } catch (error) {
      if (current !== state.chat || appSessionExpired([{ status: "rejected", reason: error }])) return;
      state.chat.pending.delete(requestID); state.chat.failed.set(requestID, { text, message: error.message || responseMessage(error.status) });
      await loadChat(true); renderChat();
    } finally { if (current === state.chat) { if (activeView === "chat" && !document.hidden) chatStream.scrollTop = chatStream.scrollHeight; schedulePoll(); } }
  }
  async function loadSession() {
    try {
      const session = await request("/api/session"); state.csrf = String(session.csrf || session.csrf_token || "");
      if (!authenticated(session)) { showLogin(); return; } await loadHome();
    } catch (error) { if (error.status === 401 || error.status === 403) { state.csrf = ""; showLogin(); return; } showLogin(); showMessage("暂时无法连接本机服务，请稍后再试。", "error"); }
  }
  async function saveHome(action) {
    if (state.busy || !state.home) return; setBusy(true); showMessage("");
    try {
      const payload = { action }; if (action !== "initialize") payload.revision = Number(state.home.revision || 0);
      await request("/api/local/home", { method: "POST", body: JSON.stringify(payload) }); await loadHome();
      showMessage(action === "initialize" ? "大肥鱼的家已经准备好。" : "工作空间授权已更新。", "success");
    } catch (error) {
      if (error.status === 409) { try { await loadHome(); } catch (_) {} showMessage("状态已经更新，已刷新当前信息。", "warn"); }
      else if (error.status === 401 || error.status === 403) { state.csrf = ""; showLogin(); showMessage("登录状态已失效，请重新验证。", "warn"); }
      else showMessage(error.message || responseMessage(error.status), "error");
    } finally { setBusy(false); }
  }
  async function saveModel(event) {
    event.preventDefault();
    const payload = { revision: Number($("#model-revision").value || 0), provider: $("#model-provider").value, base_url: $("#model-base-url").value.trim(), model: $("#model-name").value.trim(), max_tokens: Number($("#model-max-tokens").value), thinking_enabled: $("#model-thinking").checked, reasoning_effort: $("#model-reasoning").value, persona: $("#model-persona").value.trim(), api_key: $("#model-api-key").value, clear_key: $("#model-clear-key").checked };
    const button = $("#model-save"); button.disabled = true;
    try {
      state.model = await request("/api/local/model", { method: "PUT", body: JSON.stringify(payload) }); setModelForm(state.model);
      $("#model-setup-note").hidden = chatConfigured(); $("#open-settings-button").hidden = chatConfigured(); showMessage("聊天模型已保存。", "success"); setActiveView("chat");
    } catch (error) {
      if (error.status === 409 && /已更新/.test(String(error.body && error.body.error || error.message))) {
        try {
          const latest = await request("/api/local/model");
          state.model = latest;
          $("#model-revision").value = String(latest.revision || 0);
          $("#model-api-key").placeholder = latest.key_configured ? "已保存；留空不改" : "填写后保存";
        } catch (_) { /* Keep the user's draft when a refresh is unavailable. */ }
        showMessage("设置已在别处更新。已保留这次编辑，请确认后重新保存。", "warn");
      }
      else showMessage(error.message || responseMessage(error.status), "error");
    } finally { button.disabled = false; }
  }
  async function logout() {
    if (state.busy) return; setBusy(true);
    try { await request("/api/logout", { method: "POST" }); state.csrf = ""; showLogin(); showMessage("已退出登录。", "success"); }
    catch (_) { showMessage("退出登录未完成，请重试。", "error"); } finally { setBusy(false); }
  }
  $("#login-form").addEventListener("submit", async (event) => {
    event.preventDefault(); if (state.busy) return; setBusy(true); showMessage("");
    try { const session = await request("/api/login", { method: "POST", body: JSON.stringify({ password: $("#login-password").value }) }); state.csrf = String(session.csrf || session.csrf_token || ""); await loadHome(); showMessage("验证完成。", "success"); }
    catch (error) { showMessage(error.status === 401 || error.status === 403 ? "密码不正确，请重新输入。" : (error.message || responseMessage(error.status)), "error"); } finally { setBusy(false); }
  });
  initializeButton.addEventListener("click", () => saveHome("initialize"));
  accessButton.addEventListener("click", () => saveHome(state.home && state.home.home_access ? "pause" : "authorize"));
  settingsAccessButton.addEventListener("click", () => saveHome(state.home && state.home.home_access ? "pause" : "authorize"));
  logoutButton.addEventListener("click", logout); settingsLogoutButton.addEventListener("click", logout); $("#local-logout-button").addEventListener("click", logout);
  for (const [index, name] of tabs.entries()) {
    const tab = $(`#${name}-tab`);
    tab.addEventListener("click", () => { showMessage(""); setActiveView(name); });
    tab.addEventListener("keydown", (event) => {
      const positions = { ArrowLeft: (index + tabs.length - 1) % tabs.length, ArrowRight: (index + 1) % tabs.length, Home: 0, End: tabs.length - 1 };
      if (!Object.hasOwn(positions, event.key)) return;
      event.preventDefault();
      const selected = tabs[positions[event.key]];
      showMessage("");
      setActiveView(selected);
      $(`#${selected}-tab`).focus();
    });
  }
  $("#apps-refresh").addEventListener("click", () => { appFeedback(""); loadApps(); });
  $("#app-dialog-refresh").addEventListener("click", () => { appFeedback(""); loadApps(); });
  $("#tool-dialog-refresh").addEventListener("click", () => { projectToolsFeedback(""); loadProjectTools(); if (projectTools.openID === "media-understanding") void loadLocalVision(); if (deploymentDescriptor(projectTools.openID)) void loadDeployment(projectTools.openID); if (projectTools.openID === "obs-preview") void loadOBSDeployment(projectTools, true); });
  $("#tool-dialog-close").addEventListener("click", () => $("#tool-dialog").close());
  $("#tool-dialog").addEventListener("close", () => {
    const id = projectTools.openID;
    if (id === "media-understanding") { projectTools.vision.generation += 1; projectTools.vision.loading = false; projectTools.vision.saving = false; }
    const spec = deploymentDescriptor(id);
    if (spec) { projectTools[spec.state].generation += 1; projectTools[spec.state].approved = false; }
    if (id === "obs-preview") { stopOBSPreviewOnLeave(); invalidateOBSDeploymentView(); }
    projectTools.openID = "";
    $("#project-tools-view").insertBefore($("#project-tools-feedback"), $("#project-tools-list"));
    $("#tool-dialog-body").replaceChildren(); $("#tool-dialog-body").scrollTop = 0;
    const opener = [...$("#project-tools-list").querySelectorAll("button")].find((button) => button.dataset.tool === id);
    if (opener && activeView === "project-tools") opener.focus({ preventScroll: true });
  });
  $("#app-dialog-close").addEventListener("click", () => $("#app-dialog").close());
  $("#app-dialog").addEventListener("close", () => { stopDouyinWebLoginPolling(apps); stopDouyinInteractionWork(apps); stopDouyinLiveInspection(apps); apps.openID = ""; $("#apps-view").insertBefore($("#apps-feedback"), $("#apps-view .work-register")); });
  $("#project-tools-refresh").addEventListener("click", () => { projectToolsFeedback(""); loadProjectTools(); });
  $("#open-settings-button").addEventListener("click", () => setActiveView("settings")); $("#back-to-chat-button").addEventListener("click", () => setActiveView("chat"));
  $("#model-form").addEventListener("submit", saveModel); $("#chat-form").addEventListener("submit", (event) => { event.preventDefault(); sendMessage($("#chat-input").value); });
  $("#load-older-button").addEventListener("click", () => loadChat(false));
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) { window.clearTimeout(state.chat.poll); state.chat.poll = 0; }
    else if (activeView === "chat" && state.home?.initialized) void loadChat(true);
  });
  loadSession();
})();
