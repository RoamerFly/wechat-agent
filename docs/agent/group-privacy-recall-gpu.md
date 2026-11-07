# 群聊受众隔离、队列打断与 GPU 执行

## 行为

- 群聊不公开运行环境、声线模型、路径、报错与修复选项，也不把主人私聊中的选择当成群里的共同需求。跨会话读取权限不等于向群公开资料的许可。
- 当前会话上下文不自动载入其他私聊的工作区记忆或任务。管理员显式跨会话检索仍可用于完成当前账号的授权任务。
- 使用诊断能力，或草稿含诊断线索时，最终文字由模型进行受众编辑；执行前进度和 `wechat_send_text` 的群目标也走同一编辑入口。编辑仍含内部诊断时要求模型更正；无法取得合格编辑时不发布该草稿，错误进入私报流程。没有固定错误应答。
- `agent_owner_report` 把本轮技术事实排入真实主人私聊。同一账号、来源会话、执行回合只保留一个报告任务，待发期间合并补充，不重复创建。它不会把群友提升为管理员。

## 队列控制

`wechat_queue_control(action=list|pause|resume|cancel, trace_id?)` 管理当前会话的原附件队列和配套说明。普通成员管理自己的队列及本群自主分享；真实主人可以管理本会话全部队列。未知发送者的旧队列不会默认授予普通成员。

被指向的新文字暂时暂停旧附件及自主分享，模型随后决定恢复、保持暂停或取消。普通无关群消息不触发这次暂停。暂停和取消持久保存；取消尚未提交的附件直接结束，不再触发首次提交。已接受消息保留真实回执，不能靠取消队列撤回。

恢复保留原 job、幂等键和分段回执，先查询原生状态。部分原生版本把尚在 queued 状态被取消的发送任务标为不可重试；这种情况返回 `resume_error=native_cancelled_job_not_retryable` 并保持暂停，不伪报恢复、不换键创建重复任务。原生取消尚未落定返回 `native_pause_not_settled`，可稍后查询原任务。已进入上传/提交且结果未知的分段仍需核对目标会话回执，不能据此保证撤销正在进行的一次网络提交。

## 撤回

开启 Agent 的「撤回本账号消息」后，真实主人可用 `wechat_recall(message_id, request_key, thread_id?)`。启用跨会话权限后，可从私聊撤回本账号在其他已授权会话发出的消息。宿主核对账号、会话、原消息编号、发出方向和平台编号，再按当前授权提交。使用 `wechat_operation_status` 核对原任务。关闭撤回权限后，已提交的任务仍可从管理端核对。

工具已使用假原生发送器验证目标、身份、幂等及结果查询。本轮没有发送真实测试消息，也没有执行真实撤回；微信实际撤回期限与平台错误仍以原生返回为准。

## GPU 与声线

`server_exec` / `server_process` 接受 `gpu=true`；GPU 设备和驱动库由隔离执行器挂载，选择项目登记的 CUDA Python。`server_status` 区分发现的设备与本次挂载配置。未挂载 GPU 或选用了 CPU 环境时的 `cuda=False` 不能说明宿主没有显卡。

本机 Bubblewrap 实测完成 CUDA 张量计算，返回 RTX 3060 Laptop GPU 与校验和 8；同时确认宿主用户目录、用户会话目录和 Windows 系统盘未暴露。该证据属于当前 Linux/WSL 后端，不等同于 Windows LPAC GPU 验收。

指定 `music_rvc_start.voice_id` 时验证实际声线引擎与 profile。GPT-SoVITS、百炼与 RVC 权重不能混用，不能把默认声线当作指定的私人声线提交。真实主人在群中仍可读取、准备本账号私人声线；普通成员不可。准备成功不代表已具有匹配的歌声转换模型或已完成演唱。

## 验证

- 常规：`go test ./...`、`go vet ./...`、`python scripts/check_repository.py`。
- 合成回归：`TestSensitiveGroup*`、`TestQueue*`、`TestOwnerRecall*`、`TestGroupInitial*`、`TestTechnicalDiagnostic*`。
- 真实模型无发送：设置 `WECHAT_LIVE_MEDIA_ROOT` 与 `WECHAT_GROUP_PRIVACY_LIVE=1`，运行 `TestLiveGroupVoiceDiagnosticStaysPrivate`；使用合成会话和合成声线档案，报告写入测试临时目录。
- 真实 GPU 无发送：Linux 设置 `WECHAT_GPU_PUBLIC_ROOT`，运行 `TestLiveSandboxCUDAUsesGPUWithoutHostAccess`。

本机更新只替换管理端程序；保留原生微信进程、登录态、配置、聊天与素材。机器人保持用户设置的暂停状态。停止旧诊断续办前保存私有快照；私有快照不入 Git。本阶段不构建镜像。
