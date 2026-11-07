# 文档导航

从[使用说明](guide/USER-GUIDE.md)、[功能边界](guide/FEATURES.md)、[部署说明](../deploy/README.md)开始。以下主题文档保留具体实现和证据边界，文内版本与日期表示当时验证范围。

## 使用与扩展

- [多账号管理与重新扫码登录](guide/MULTI-ACCOUNT.md)
- [目录与启动入口](guide/project-layout.md)
- [Windows 独立家目录预览与启动](guide/local-home-preview.md)
- [本地聊天的音频工坊](guide/audio-workshop.md)
- [运行服务、Agent权限与可迁移性](guide/runtime-and-portability.md)

- [能力和验收边界](guide/FEATURES.md)
- [大肥鱼：朋友式人设、好友私聊与成员提醒](guide/friendly-persona-friends-reminders.md)
- [API / MCP 工具配置与开发](guide/PLUGINS.md)
- [使用大肥鱼](guide/USER-GUIDE.md)

## Agent运行与权限

- [大肥鱼长期成长阶梯计划与实施进度](design/大肥鱼长期成长阶梯计划.md)
- [Agent-home-station 应用接入与项目工具规范](design/应用接入与项目工具规范.md)
- [账号独立 Agent、受管工作区与跨会话附件](agent/managed-workspaces.md)
- [离线未读汇总与任务收尾](agent/unread-conversations-and-task-history.md)
- [上下文传递与歌曲策略](agent/context-and-song-policy.md)

- [自主修复和本机程序启停验收](agent/agent-self-repair-acceptance.md)
- [Agent 会话执行内核（0.25.0）](agent/agent-session-runtime.md)
- [Agent 工作台精简](agent/agent-workbench.md)
- [自主续聊与可续执行任务](agent/autonomous-continuity.md)
- [消息判断与发送节奏](agent/decision-and-send-pacing.md)
- [自主工作权限与实机验收](agent/full-agent-acceptance.md)
- [本地历史归档与容量](agent/history-archive.md)
- [任务、工具会话与上下文优化检查点](agent/lifecycle-upgrade.md)
- [工具能力暴露与按需读取](agent/open-tool-capabilities.md)
- [工具健壮性与隐藏执行](agent/tool-resilience.md)
- [统一 Agent 工作区与执行循环](agent/unified-agent-runtime.md)

## 消息投递与恢复

- [自主行动和发送恢复](delivery/autonomous-delivery.md)
- [不确定文字发送的持续恢复](delivery/delivery-retry.md)
- [媒体关联、转换链与完整复用](delivery/media-lineage-upgrade.md)
- [更新等待与自动恢复](delivery/update-drain-recovery.md)

## 媒体、歌曲与分享理解

- [蓝色大肥鱼：百炼专用音色候选](media/bailian-character-voice.md)
- [视频号授权读取](media/channels-authorized-reading.md)
- [新转发视频号自动读取研究目标](media/channels-automatic-reading-goal.md)
- [微信视频号实际理解：实现与验证边界](media/channels-content-understanding.md)
- [视频号后台读取候选核实（2026-10-25）](media/channels-source-options.md)
- [媒体与后续说明的处理顺序](media/media-caption-observation-order.md)
- [分享实际内容观察](media/share-observation.md)
- [C声线歌声改进与验收](media/singing-voice-refinement.md)
- [翻唱的歌词、旋律与节拍对应](media/cover-phrasing-comparison.md)
- [歌曲请求、参考原曲与歌词依据](media/song-request-integrity.md)
- [歌曲默认声线与本地桥接恢复](media/song-voice-policy.md)
- [自主冲浪的分享决定与记录](media/surf-sharing-outcomes.md)
- [YuE参考翻唱、种子与歌曲来源](media/yue-reference-cover.md)
- [本地 YuE 服务运行与故障恢复](media/yue-service-recovery.md)

## 管理端与目录

- [管理后台重构 · 2026-10-27](ui/admin-console-design.md)
- [管理端排版与遮挡修复](ui/admin-layout-review.md)
- [群成员目录一直显示“正在查询”的修复](ui/catalog-member-loading-fix.md)
- [刷新群聊与好友](ui/catalog-refresh.md)
- [会话与记忆工作区](ui/conversation-workspace.md)
- [群成员名称查询排查与恢复](ui/member-directory-recovery.md)
- [可编辑流程工作台](ui/node-workbench.md)

## 手机探索

- [持续目标：手机 Agent 与微信后台结合交付](mobile/hybrid-device-agent-goal.md)
- [大肥鱼：微信后台与手机 Agent 结合方案](mobile/hybrid-device-agent-plan.md)
- [手机执行端：ADB stdio MCP](mobile/mobile-bridge.md)
- [手机探索工作台](mobile/mobile-workbench.md)

## 设计参考

- [QQ／微信 AI Agent 自主网上冲浪：多源内容发现、兴趣漫游与智能分享方案](design/QQ微信_Agent自主冲浪多源探索与内容分享方案.md)
- [QQ／微信 AI 真人式聊天：多段消息与自主连续发言实现机制](design/QQ微信_AI真人多段消息与自主发言机制.md)

- [跨平台运行与镜像构建方案](../镜像构建方案.md)：当前本机优先、连接器/工具拆分、新机环境与后续镜像更新。
