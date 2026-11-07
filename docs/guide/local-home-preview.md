# Windows 独立桌面预览

独立入口可以在没有微信账号、原生端、ADB 或模型运行环境的安装中启动。主人完成本机身份准备、保存模型连接后，即可在软件页面与同一条大肥鱼聊天。它复用现有聊天引擎和 SQLite 记忆内核，不把本机主人伪装成微信账号。

页面左侧场景跟随窗口高度，右侧内容独立滚动。「设置」中的主角色用于本机聊天和各应用的基础性格。应用页点击微信、抖音等卡片，在大弹窗中管理账号与使用范围、应用子角色和源码。应用子角色可以填写该平台的语气、兴趣或偏好，留空沿用主角色；群备注仍只作用于对应群。修改子角色在下次模型调用生效，无需重新登录应用。

项目工具页使用等宽等高卡片，点击后在大弹窗内管理安装、启停、声线、工作流与资源。OBS 预览随工具弹窗关闭而停止。桌面图标与启动、停止画面采用暖灯小屋标识。关闭窗口时可选择“收起到托盘”或“退出并停止服务”，勾选“记住这次选择”后沿用所选方式。

这是 Windows 开发预览，正式程序切换情况以阶梯计划的最新验收记录为准。独立入口可在桌面窗口或本机 HTTP 服务中运行。Agent 可以在自己的家目录读写文件、持久保存工具，并通过受管 Windows 运行器执行程序。应用页支持源码安装、微信账号登记与扫码、会话授权、Agent 启停及回执查询。抖音提供应用内授权窗、授权续期、作品读取与发布接入，各项真实平台验收范围见阶梯计划。

抖音提供两条独立入口：**普通账号网页登录**无需开放平台密钥，操作见本页后面的网页登录说明；**开放平台网站应用授权**用于正式 API。后者从[开放平台控制台](https://developer.open-douyin.com/console)开始：完成开发者主体认证，创建网站应用，审核通过后在应用信息取得 Client Key、Client Secret，并登记自己控制的 HTTPS 回调地址。密钥只填写在软件的私有配置表单中，不发到聊天或提交仓库。[官方创建步骤](https://developer.open-douyin.com/docs/resource/zh-CN/dop/develop/app-mgmt/create-mobile-and-web-app)

2026-11-07 核对的[官方入驻条件](https://partner.open-douyin.com/docs/resource/zh-CN/developer/join/issues)列明：企业、个体工商户等主体可创建网站应用；个人主体支持小程序、小游戏、小玩法。仅有普通个人抖音账号，不能因此认为已具备网站应用资格或接口权限。软件在开放平台未配置时展示申请入口与这一前置条件；普通网页登录不受这项 API 申请前置条件限制，真实平台验证范围以阶梯计划为准。

## Windows 桌面预览

在源码目录构建到全新的隔离目录：

```powershell
./scripts/Build-Desktop.ps1 -LocalStation -OutputDirectory temp/local-station-preview
```

双击输出目录里的 `AgentHomeStation.exe`，窗口显示本机聊天、应用和设置页面。此构建不要求微信原生程序、ADB、Python、GPU 环境或任何应用账号，也不拷贝原来的私有配置。Windows 仍需 WebView2 Runtime；构建需要源码开发环境。

验收使用另一个独立输出目录，加上 `-Test` 会生成合成测试资料和测试程序，不应将这份测试资料当作自己的日常安装。该模式包含两轮真实服务启停，以及私有桌面的实际 WebView 加载与退出，不向用户任务栏弹出测试窗口。

预览沿用现有最大化、普通最小化、关闭选择、托盘及单例逻辑。其单例与控制通道同旧 `WechatAgent.exe` 分开，便于迁移期间并存；预览停止操作不会停止旧微信应用。正常退出通过继承的私有管道通知子服务，先停止接收新聊天、取消当前生成，再关闭自有进程和存储。没有公开的 HTTP 停止接口。

默认私有资料在包目录 `data/station`，家目录根在其下的 `managed-workspaces`，与直接启动独立服务的默认值一致。桌面通过现有管理登录取得本机会话，并核对服务 PID 和私有目录匹配自己启动的子进程；端口被占用时拒绝接管。

需要自定义目录时，可在这个预览包自己的 `secrets/local-deployment.json` 配置：

```json
{
  "backend": "local-station",
  "data_root": "data/station",
  "workspace_root": "data/station/managed-workspaces",
  "admin_url": "http://127.0.0.1:8082"
}
```

路径相对预览包目录解析。不要将该配置写进原微信安装，也不要指向真实资料进行试验；生产迁移仍需后续应用接入步骤。未知运行结果不会在重启时自动重放；模型可按原任务编号查询，尚不自动唤醒续办。

## 启动与配置

```powershell
go build -o temp/local-home/agent-home-station.exe ./cmd/agent-home-station
& ./temp/local-home/agent-home-station.exe
```

打开 `http://127.0.0.1:8082`。成长站和桌面自动登录使用相同顺序：先读站点私有目录的 `admin-password`，其次使用非空的 `AHS_ADMIN_PASSWORD`，最后读 `admin-token`。空的测试安装由服务首次写入默认口令 `admin`。环境变量口令不落盘，已有凭据继续原地读取。

设置目录默认在可执行文件旁的 `data/station`，工作空间默认是其下的 `managed-workspaces`，均不受启动时当前目录影响。需要自定路径时使用绝对路径：

```powershell
& ./temp/local-home/agent-home-station.exe `
  -data 'E:\AgentHomePreview\host' `
  -workspace 'E:\AgentHomePreview\workspace' `
  -listen '127.0.0.1:8082'
```

该示例建立的是隔离测试安装，不表示原来的大肥鱼已经迁移。真实安装的身份、配置和消息尚未自动搬动。

进入后先准备家目录，再在「模型设置」保存 API 服务商、HTTPS 地址、模型名称和 Key。默认使用 `deepseek-flash`、开启思考、强度 `low`。设置保存本身不测试连接、不调用模型，只有你发送消息后才请求服务。OpenAI 兼容接口是否支持思考以提供商协议为准；当前适配器只向 DeepSeek 发送这套思考参数。

模型密钥保存在宿主的 `local-model.json`，页面显示是否已配置，留空保留原值，显式勾选才清除。首次接续命令可以把主人选定的旧模型设置写入这份私有登记。后续保存使用版本条件，发生冲突时保留编辑草稿，先核对最新配置再保存。

## 聊天、原文与失败恢复

- 本机消息的来源为 `local`，归属于稳定鱼身份及本机主人。历史按实际发生顺序展示，先显示最近一页，再按游标读取更早记录。
- 输入和实际保存的回复使用原有 SQLite 事件表。近期上下文是选取窗口，较早原文可通过 `memory_search`、`memory_read` 按需取回；工具不能选择其他微信账号或会话。
- 每次发送有稳定请求编号。相同编号、相同原话在回复已保存后再次提交，直接返回原回复，不再次调用模型。编号相同而正文变化会被拒绝。
- 回复气泡整组保存，不把只生成、尚未保存的文字显示成已完成。这里的完成表示已写入本地聊天记录，不代表微信发送、手机播放或主人已经阅读。
- 模型失败保留用户原文，页面提供同编号的手动重试。重启不自动重放未完成请求；服务端有原记录时可以继续核对。浏览器网络不确定时先读真实历史，已有回复即清除失败状态。
- 文件写入、修改和 Windows 进程启动会先登记到原请求。重试会提供最近八项操作和总数，进程结果从原作业登记查询。已有操作或缺少新版检查点的旧请求可以续查、取消原进程。需要新增写入或启动时，请发一条新请求。
- 忘记或更正来源后，旧请求和迟到结果不能绕过原来的记忆屏障。宿主关闭会取消正在生成的本地回合。
- 模型请求用量写入独立的 `local-billing`，沿用提供商报告/未知的区别。新建本地聊天目前不自动启动额外摘要、维护或复核调用，不把估算当作账单。

原始事件会留在 SQLite 中，近期窗口之外不等于删除。此步接通的是对话记录和按需读取；自动长期条目整理、主动行为与生活视图尚未接入这个独立入口。

## 家目录与旧服务

点击准备后，宿主的 `fish-identity.json` 保存鱼身份、本机主人与授权版本。家目录为：

```text
<workspace>/fish_home/<完整 fish_id>/home/
  diary/
  projects/
  works/
  scratch/
  Project Tools/
  Connected Apps/
```

聊天与家目录执行授权是独立的。暂停工作空间不会禁止主人继续聊天；此步的授权仍未挂到旧微信 Agent 的文件执行路径上。`Connected Apps` 初始为空，通过应用页安装后会出现相应源码目录。

### 应用

在独立预览中准备好家目录，打开「应用」。要试装微信源码，构建时加 `-WithWeChatApp`：

```powershell
./scripts/Build-Desktop.ps1 -LocalStation -WithWeChatApp -OutputDirectory temp/local-station-with-wechat
```

在微信一栏选择「安装源码」。发行源核验通过后，文件复制到家目录的 `Connected Apps/wechat`。源码可编辑，重复安装同一版不会覆盖工作文件；检测到同名目录或不同版本时，应用页会提示保留已有内容后再整理。暂停工作空间期间可查看安装结果，恢复授权后可安装。

「账号连接」单独读取当前宿主的登记。普通预览包未随附微信源码时，仍可使用本地聊天。

### 可选的微信原生预览

用此前构建并核验过的 Windows x64 运行组件制作单独预览包。构建器同时加入应用发行源码：

```powershell
./scripts/Build-Desktop.ps1 -LocalStation -WithWeChatRuntime `
  -WeChatRuntimeDirectory temp/verified-native/runtimes/windows-x64 `
  -OutputDirectory temp/local-station-native-preview
```

只在新预览包自己的 `secrets/local-deployment.json` 中加入 `wechat_native_url`。它与管理端口必须不同：

```json
{
  "backend": "local-station",
  "wechat_native_url": "http://127.0.0.1:6184"
}
```

桌面核对随包运行文件后启动本实例的微信原生进程。私有目录固定为本机预览数据根下的 `apps/wechat/native`；工作空间中的 `Connected Apps/wechat` 仍只存可编辑发行源。应用页显示进程就绪、已登记账号和登录状态。新预览实例初次启动没有账号。主人可在「应用 → 微信 → 账号连接」添加账号，再在该账号旁点「扫码登录」，用手机微信扫码和确认。二维码过期后可重新发起；暂时无法确认请求结果时，同一页面重试会沿用原请求编号。已登录或正在恢复的连接不会被新的扫码请求替换。每个账号可单独设置「回复范围」，填入原微信会话 ID（每行一个，文件传输助手使用 filehelper）。群聊默认在被提及时回复，已启用自动总结的群还会处理符合条件的视频号分享。文件传输助手使用 /ai 前缀，获准的普通私聊可以直接对话。设置本机模型并保存范围后即可启动该账号的 Agent；未登录时显示等待登录。修改运行中连接的范围会先暂停，核对后可手动启动。清空范围可撤销回复授权。暂停保留微信登录、账号队列及原回执，并在重启后保持暂停。独立预览已覆盖文字回复与单独授权的入站附件取得。旧账号可按下节预演接入。

在同一连接的「设置回复范围」中，可另填允许读取附件的会话 ID，范围须包含在回复会话里。获准会话的 Agent 在处理原消息或已核验引用时可发现 `wechat_file_read`。工具只返回素材编号、类型、大小、校验信息及微信实际提供的版本。缩略图不会登记为原件。撤销后旧编号失效；原件仍保存在宿主私有目录。

回复范围中的每个群可填写「群备注」，用于指导该群的表达与回复。将该群加入「附件读取会话 ID」后，还可以单独开启「自动总结视频号分享」。配置并启用媒体理解工具后，新收到的视频号卡片会自动读取实际口述或画面，再决定如何总结。读取未得到有效内容时，本轮不会在群里回复。撤销附件读取会关闭自动总结并保留备注，重新授予读取权限后需要再次开启。旧版备注中的分享偏好需要在这里明确设置。修改运行中的群规则后可在应用页重新启动该连接。

主人也可以使用已登录的管理会话调用 `POST /api/apps/wechat/connections/{connection}/attachments`，带上 CSRF 和 `thread_id`、`message_id`。使用返回的 `asset_id` 请求 `GET /api/apps/wechat/connections/{connection}/attachments/{asset}/content` 下载原件。每次取得均校验微信连接及原消息归属，下载内容需通过大小及 SHA-256 核验。单件上限为 512 MiB。此步骤验证原件取得；内容解析、媒体外发和真实账号的收取验证分开进行。

要让大肥鱼把本轮附件发回原会话，将该会话同时加入「附件读取会话 ID」和「附件回复会话 ID」，保存后启动该连接。在已授权会话中明确请求回复当前或引用的附件即可。`wechat_attachment_reply_prepare` 生成本轮可选编号，实际上传和发送在模型选用后由队列处理。语音沿用原生分段，停止请求保留已接受片段。按原提交键读取 `/api/apps/wechat/connections/{connection}/receipts/{key}` 可核对原任务。`media_reply_threads` 是独立的授权字段，显式提交空数组可撤销，撤销上级读取范围也会撤销相应的附件回复。

在应用页展开「登记家目录中的作品」，填写成品相对路径即可得到作品编号。也可让大肥鱼在本地聊天中用 `artifact_publish(path, expected_sha256)` 登记。作品保存为宿主私有快照，家目录中的原文件后续可继续修改。

在连接的回复范围编辑区展开「授权作品交付」，填写作品编号、已授权的接收会话和请求者微信 ID。保存后，该成员可在指定会话向大肥鱼索取作品。大肥鱼用 `wechat_work_reply(action=list)` 查看本人的授权，用返回的 `delivery_id` 准备文件回复。列表默认每页 20 项，可指定 limit 并按 next_after 继续。

「查看已有授权」可在重启后查看或撤销原授权。撤销会暂停该连接。重新授权使用新的请求编号，避免旧请求复活。家目录暂停期间不能交付作品，恢复后需重新授权。微信回执查询沿用原投递键。

在微信连接下展开「投递记录」，查看当前工作器保存的文字与媒体投递状态。列表显示已接受、待核对及已取消的分段，未确认的在途分段不会提前算作取消。「核对回执」只查询原投递键或原生任务，不生成内容、不重新发送。当前列表不包含旧版导入的只读归档；归档回执仍可通过原编号接口查询。

要撤回已接受的消息，在投递记录中找到该消息。若有多个已接受片段，先选择要撤回的那一段。点击「定位可撤回消息」，必要时继续查找，再明确确认撤回。若原生端没有提供可核对的消息或拒绝撤回，页面会保留相应状态。刷新只查询原撤回操作，没有原生操作编号时显示结果待核对。停止剩余投递仍使用独立的「停止本回合投递」。

「停止本回合投递」按该记录的原请求停止剩余媒体、文字气泡及配文，同群其他回合继续。即使选中的气泡已经接受，也可停止同一回合尚未提交的后续内容；已接受消息不会自动撤回。停止意图先写入宿主私有日志，再核对并取消原生任务。上传中的旧结果会丢弃，结果未知时继续按原键或原任务核对，重启不会解除停止。界面先显示「停止意图已保存」，通过核对回执查看实际进度。不能通过反复启动 Agent 恢复已取消任务。

停止接口为 `POST /api/apps/wechat/connections/{connection}/receipts/{key}/cancel`，要求本机主人登录和 CSRF。成功保存返回 202 和 `cancellation_requested`，不表示所有在途片段已经取消。日志位于该连接宿主私有目录的 `delivery-stops.json`，不应手动删除以恢复投递。原生暂时不可用时，停止意图继续保留，由工作器补查原任务；已暂停的工作器会在下次启动时先消费停止记录。

聊天工具 `wechat_queue_control(action=resume, trace_id=原回合编号)` 可在有新明确续发指令时续办已取消的分段语音。此路径先重新读取原生回执，只接受完整、连续的分段清单，且已接受片段在前、未提交片段在后。原任务未取消、仍有在途片段、清单不全、来源或权限变化时，返回具体边界，不重新发送。

主人也可在该连接的投递记录中点击「续发未发送片段」。请求先存入宿主私有的 `delivery-resumes.json`，由工作器在扫描已有入站消息后处理。暂停或离线时可保存，恢复运行后才核对分段和实际执行。同一原投递的重复点击、刷新和重启返回同一请求；保存成功不代表已经续发。分段仍待核对时按随机退避继续查询。原文件从未提交为分段任务时不会拿续办接口重新发送整个文件，页面显示具体原因。

续办登记接口为 `POST /api/apps/wechat/connections/{connection}/receipts/{key}/resume`，提供稳定的 `request_key`，要求主人会话和 CSRF。返回 202 及 `continuation` 状态：请求已保存、等待原任务回执、已建立续发任务、暂不能续办或续办已停止。原投递的列表和单条回执包含同一状态，`delivery_key` 指向已有续发任务；页面「核对回执」同时核对这个关联任务。原任务停止记录保留，不能通过移除它来实现续办。

保存续办后，可点击「停止续办」，它会停止原回合派生的待办续发，并继续核对已提交片段。即使工作器结果迟到，也不能把已停止请求改回待执行。同一原任务只有一次续办记录；若这次续发后来又被取消，需要对新续发记录提出新的明确续办操作，而不是反复点击原记录。

续办使用独立的新投递键，并在调用原生「只续发缺失部分」接口前保存。程序在提交前重启，可在再次核对原回执后继续首次提交；已经进入调用而响应丢失时，只按新键核对，查不到也不会改走普通发送或重发原音频。重复续办同一原任务会返回已有续办记录。投递列表中的「语音续发」及接口 `resumed_from` 保留原键关联；原任务的停止记录和已接受分段继续保留。

第一次保存此类续办意图时，当前工作器队列的内部版本升级为 2；原文件名仍是 `messages-v2.json`。新版兼容内部版本 1 和 2，旧版工作器会拒绝版本 2，避免将续办状态当成完整发送。启用新版续办后，不要让旧程序接管该队列；故障时保留数据，由兼容版本查询或修复。

列表接口为 `GET /api/apps/wechat/connections/{connection}/receipts?limit=20&after=...`，要求本机主人登录。limit 为可调整的正整数；next_after 用于按投递编号继续查看。响应的 coverage 为 `current_worker_checkpoint`，snapshot 标识本次原子快照。翻页期间快照发生变化时，页面提示重新查看，避免混合两个版本。列表只返回投递元数据，不返回草稿正文、媒体路径或凭据，也不轮询原生服务；单条「核对回执」才查询平台任务。

管理接口提供 `POST /api/local/artifacts`，参数为 path、request_key 和可选 expected_sha256；`GET /api/local/artifacts/{artifact}` 返回成品元数据。作品交付授权使用 `POST /api/apps/wechat/connections/{connection}/work-grants`，提供 artifact_id、thread_id、requester_id、request_key 和 enabled。`GET` 同路径列出授权，可用 limit、after 分页；`POST` 其下的 `{delivery}/revoke` 撤销原授权。接口均要求本机主人会话，写操作还需 CSRF。

### 首次启动前接续旧模型与鱼身份

先关闭旧桌面及其服务。选择全新的成长站数据目录，在首次启动之前运行下面的预演。使用日常安装包，测试构建产生的合成资料应留在测试目录。

```powershell
$stationPackage = 'E:/AgentHomeStation'
$stationServer = Join-Path $stationPackage 'agent-home-station-server.exe'
$legacyAdmin = 'E:/OldStation/data/admin'
$stationData = Join-Path $stationPackage 'data/station'
New-Item -ItemType Directory -Force -Path (Split-Path $stationData -Parent) | Out-Null

$previewJSON = & $stationServer --data $stationData --legacy-settings-source $legacyAdmin
if ($LASTEXITCODE -ne 0) { throw '旧设置预演失败，请核对提示' }
$preview = $previewJSON | ConvertFrom-Json
$preview
```

预演报告显示模型、输出额度、思考开关与强度、密钥是否已配置，以及已有鱼身份。核对后用原摘要确认。

```powershell
& $stationServer --data $stationData --legacy-settings-source $legacyAdmin --legacy-settings-digest $preview.digest
if ($LASTEXITCODE -ne 0) { throw '旧设置接续失败，请核对提示' }
```

| 旧记录 | 接续结果 |
| --- | --- |
| `config.json` 的服务商、地址、模型、密钥、输出及思考设置 | 写入新站私有 `local-model.json`，保留原设置 |
| 系统提示及角色、表达风格、关系设定 | 仅合并进微信应用子角色，相同文本合并一次；主角色使用独立默认值 |
| 存在且可核验的 `fish-identity.json` | 保留原鱼身份、主人与绑定记录 |
| 输入内容与预演摘要 | 私有接续记录保存摘要及文件校验信息，供重复执行核对 |

预演读取来源，确认操作在临时目录写入并核验后原子启用目标。摘要随来源或目标变化而失效。相同输入和未变化的已导入结果可以重复核对。已有其他资料的目标会拒绝合并。命令结束后再启动新成长站，完成本机登录与家目录核对。

家目录文件、微信登录及会话记忆、声线和其他工具资源分别接续，按下方入口逐项核对。旧模型与身份的接续需要一个全新数据目录，应在首次启动新站前执行。接续完成后再使用该目录初始化家目录、登记工具及接入应用。

### 普通抖音账号登录

在「应用 → 抖音 → 账号」选择「打开网页登录窗口」，确认使用本项目的专属登录资料。窗口没有浏览器地址栏，但仍显示抖音网站内容；扫码、短信或验证操作由你在窗口内完成。完成后回到软件点击「我已完成登录，保存」，软件等待窗口正常退出后保存确认时间。手动关掉窗口后也能点击保存。取消只结束本次登录，不会把它标记为已确认。

以后登录过期，使用「重新打开登录窗口」继续同一份资料。需要换账号或清除时，选择「移除网页登录资料」并确认。只移除项目自己的网页资料，不清理日常浏览器，也不影响另一条开放平台授权。程序重启不会自动打开登录页，未完成的登录需要重新打开。

“网页登录资料已保存”不等于平台确认账号在线，实际有效性以网页为准。这条入口无需开放平台密钥；下方「开放平台授权」用于官方 API，是独立的可选接入。登录窗口最长保留十分钟，超时或程序停止后关闭；尚未确认时不显示保存成功。

保存后可选择「允许读取首页文字」，让本地聊天按需发现抖音首页的网页读取能力。默认仅首页；开启后可在「读取范围」明确改为「首页及视频页文字」。扩大范围后，大肥鱼会看到首页实际出现的视频候选，自行选择跟进它们的网页文字。它不能输入任意网址或猜视频编号取得访问。读取文字不能据此认定已经听过音轨或看完视频；点赞、评论与发布仍是另外的能力。默认不开启，不会因登录成功就后台读取。重新登录或移除资料后需要再次允许；停止读取后，已缓存内容也不能用于新的回复。页面要求登录或验证时，重新打开网页登录窗口处理即可。

需要继续读取同一页文字时，大肥鱼可按返回的进度继续调用；默认每次 2048 字、最多 8 个视频候选，标题也占用字数。可显式调整字数以读取更多候选，全部候选与正文均可续读。需要最新首页时使用刷新读取，它会建立新快照。登录窗口尚未关闭、网页要求验证、读取超时、正文过大及本机存储不可用会分别报告原因。升级网页读取格式后，旧快照仍保留，但续读需要从新快照开始。

在「首页及视频页文字」范围下，可以另勾选「允许静音观察视频播放」。这允许大肥鱼对已选视频进行短暂后台静音采样；软件会核对视频编号与主播放器媒体，记录实际播放了哪些区间，不占用你的鼠标键盘。回到仅首页、停止网页读取、重登或移除资料时自动停止这项许可。默认每次 6 秒，可请求 1–20 秒；同次采样可以复用，明确刷新才重新采样。媒体对应不明时会显示具体原因，无法核对的 blob/MSE 播放器当前不返回进度。

需要读取视频讲话时，可另选「允许本机转写视频音轨」。先在项目工具中启用本地媒体识别并登记模型资源，资源未就绪时不会保存许可。转写可以独立开启，无需同时开启播放观察。大肥鱼从已核对视频的完整原件按段取音轨，默认每次 60 秒，可调整为 1–240 秒并继续读完；已有原件和转写可复用，明确刷新才重新读取。没有音轨、没有人声、下载失败和识别服务故障分别显示，静音视频不会一律报成识别失败。

视频页文字、音轨转写和播放记录分开使用。即使普通网页采用 blob/MSE 播放器，只要精确视频详情能提供可获取的完整原件，就可以尝试本机转写；这不说明浏览器播放链已经支持该媒体流。当前尚未接入普通网页的画面理解。本轮以本地合成页面、媒体和识别替身验证流程，真实抖音加载、账号挑战、CDN 获取及转写质量仍需另行验证；尚未提供内嵌二维码、普通网页点赞、评论或发布入口。

### 接续选定的旧项目和笔记目录

先完成本机主人和家目录初始化，在工作空间授权有效时接续资料。逐项选择旧项目、作品或笔记子目录，保留来源中的原文件。目标使用鱼家目录内尚不存在的相对目录，如 `projects/legacy-public`。

```powershell
$stationServer = 'E:/AgentHomeStation/agent-home-station-server.exe'
$stationData = 'E:/AgentHomeStation/data/station'
$oldProject = 'E:/OldStation/managed-workspaces/public_workspace/projects/example'
$target = 'projects/legacy-public'
$budget = 1073741824

$previewJSON = & $stationServer --data $stationData --legacy-home-source $oldProject --legacy-home-target $target --legacy-home-max-bytes $budget
if ($LASTEXITCODE -ne 0) { throw '项目接续预演失败，请核对提示' }
$preview = $previewJSON | ConvertFrom-Json
$preview

& $stationServer --data $stationData --legacy-home-source $oldProject --legacy-home-target $target --legacy-home-max-bytes $budget --legacy-home-digest $preview.digest
if ($LASTEXITCODE -ne 0) { throw '项目接续失败，请核对提示' }
```

自定义过工作空间根目录时，两条命令都需传入同一个 `--workspace` 绝对目录。字节预算为本次可配置值。超过预算时增加预算或拆选更具体的子目录，再重新预演。复制按实际文件大小和 SHA-256 校验，来源变化后原摘要失效。

预演摘要绑定鱼身份、本机主人、家目录授权版本、来源及目标。目标发布后重复执行核对同一结果。后来编辑过的目标不会被旧资料覆盖。Windows 会核对所选目录在本机磁盘上的实际路径。选择会话、旧素材、临时上传、凭据或声线目录的下级也会被拒绝。目录链接及已知的宿主控制文件同样会被拒绝。检查依赖已知的目录和配置标识，来源须由主人明确选定。会话使用授权内的旧会话接续，语音模型和声线库使用专门的资源登记流程。

### 在隔离副本预演旧版微信接入

1. 正常排空并退出旧桌面服务，保留其原安装。将需要核对的旧宿主数据做成单独的停机快照，保存在本机私有目录。快照包含旧数据根下的 `admin/config.json`、可选的 `admin/accounts.json` 和各账号配置，以及 `bot-state`。新成长站数据根、鱼家工作空间、旧快照分别放置。支持切换记录的旧桌面正常退出后会在旧数据根生成 `legacy-cutover-state.json`。把这份文件原样复制到快照根，保留原目录中的记录供预演比对。
2. 在两个原生服务都已停止时，使用各自的 Windows `wx.exe` 走原生支持的档案导出和导入。保留旧账号的原始 ID，选择新原生目录 `data_root/apps/wechat/native`。导入时该编号须为空，已有同编号档案时先核对归属。使用导出档案路径，不直接复制保存了绝对路径的原生注册表。不要把导出档案放进大肥鱼家目录或 Git。

   ```powershell
   & $oldWx --json --data-dir $stoppedNativeRoot --account $account store export --destination $privateExport --source-stopped
   & $newWx --json --data-dir $stationNativeRoot --account $account store import --source $privateExport --source-stopped
   & $newWx --json --data-dir $stationNativeRoot store list
   ```

   如需在接入页面核对档案，把成功的 `store import` JSON 输出按 UTF-8（无 BOM）保存为新数据根下的 `apps/wechat/native-import-<连接 ID>.json`。以主人身份读取 `GET /api/apps/wechat/connections`，找到 `account_id` 匹配的记录，取该记录的 `id` 作为连接 ID。先把导入输出保存在本机私有目录，再在目标连接出现后按其编号命名。

   查看导出回执的完整性标记。默认流程只搬原生档案，目标登录仍需重新核对；先在预演中用合成或空账号。档案本身可能含官方客户端资料，应与原备份一样隔离保存。原生发送账本与宿主待处理任务有各自的来源，导出档案不自动包含它们。新账号可能初始化空的控制库，核对其中没有旧发送任务。遇到 `profile_projection_pending`，错误会列出同步批次、游标、登录材料、媒体等待写回状态。先解决相应状态，再正常导出。`--allow-incomplete` 的产物保留不完整标记，不能据此宣称完整接续或登录已恢复。

3. 在新数据根的 `apps/wechat/legacy-import-source.json` 写入下面的本机私有配置。主账号使用旧桌面部署配置的原账号 ID。次账号按旧账号登记表核对，已删除账号无法导入。

   ```json
   {
     "snapshot_root": "E:/AgentHomePreview/private-legacy-snapshot",
     "legacy_host_root": "E:/OldWeChat/private-data",
     "primary_account": "desktop",
     "snapshot_stopped": true
   }
   ```

   使用 `legacy-wechat-snapshot` 制作的新快照时，再加入 `"snapshot_manifest_digest": "<--verify 返回的 manifest_digest>"`。这是源快照清单的固定摘要，与下面界面返回的接入预演 `digest` 分开。旧手工快照可以省略此字段。

4. 启动独立桌面，以本机主人登录。在「应用 → 微信」的对应连接展开「从旧版接入」，先预演，核对账号、建议回复范围、需要人工核对的规则及旧回执计数，再确认接入。已有工作记录会提示冲突。快照发生变化时须重新预演。预演和确认接入也可分别通过 `GET` 与 `POST /api/apps/wechat/connections/{id}/legacy-import` 执行，写请求携带预演摘要 `digest` 和 `X-CSRF-Token`。
5. 导入后账号保持暂停，旧权限显示为建议。先在「应用 → 微信」保存明确的回复范围，再核对本节的启用条件。不同原生数据集只显示旧回执快照。原生数据集一致时，可以按原操作编号只读核对迟到结果。旧消息正文、模型密钥、活跃队列和执行游标不进入新工作器。

预演与「查看接入摘要」都显示最新的切换前置检查：

- **旧宿主**核对快照内的停机记录与原目录记录。支持该记录的旧桌面再次启动会使旧记录失效。`legacy_host_root` 为可选的原数据根，必须与快照及新宿主目录分开。没有记录时仍可保存预演摘要，停机继续标为待核对。由其他版本或外部程序启动的实例仍需主人单独排查。
- **原生档案**核对导入回执、当前受管档案目录及目标连接的本机原生账号 ID。档案就绪时，登录仍可处于未登录或离线状态。
- **目标登录与身份**区分未登录、已认证但未在线、在线和待核对。主人仍需在切换前确认实际登录的是同一个微信身份。保存摘要会保持暂停及空授权，不会切换服务。

**核对旧会话记忆。** 把旧端正常关闭后的一致 SQLite 副本放进同一隔离快照。主账号使用 `admin/companion/memory.sqlite`，其他在册账号使用 `admin/accounts/<账号 ID 的 SHA-256>/companion/memory.sqlite`。请保留旧文件。如果副本目录存在 `-wal`、`-shm` 或 `-journal`，应重新制作一致副本。直接删掉伴随文件可能丢失已提交内容。

从对应连接的「从旧版接入」点击「核对旧会话记忆」。报告按会话区分来源候选、排除项、有来源的活跃记忆与摘要存在性。来源候选统计当前保留且可归属的用户原话与管理员更正。排除项统计可归属但已屏蔽、遗忘或存在来源冲突的陈述。同账号且会话身份可以核对的记录才显示会话编号。无法映射与其他账号、其他渠道只显示数量。原文、摘要内容、历史压缩包、待办和权限都不会进入新连接。页面上的历史授权是新连接当前的设置，后续接入旧记忆仍需单独授权与确认。只存在旧 `memory.json` 时提示格式暂不支持。

接口为主人会话下的 `GET /api/apps/wechat/connections/{connection}/legacy-import/memory`。默认每页 30 条，可设 `limit=1..200`。用返回的 `next_after` 作为下一页的 `after`。副本或目标授权变化导致游标失效时，从第一页重新核对。

**接续选定会话。** 先保存旧版接入摘要，核对旧端的停机记录和目标原生档案导入回执，确认目标微信在线，再为该会话单独授权历史读取。暂停连接并等待 Agent 退出后，打开「核对旧会话记忆」，为相应会话预演接续。预演只显示数量。确认实际微信身份相同后才提交。来源、授权或原生服务实例变化时重新预演。目标会话已有新记忆时提示冲突，其他会话可分别核对。

接续只保存来源与版本能核对的原始陈述和仍有有效引用的旧记忆。资料保存在目标连接的宿主私有目录。恢复 Agent 后，大肥鱼可以在这个会话中按需调用 `wechat_read_legacy`，旧资料会带上来源与版本。撤销历史授权后无法继续读这份资料，重新授权也不会恢复旧授权下的接续记录。原有实时记忆、发送账本与待办继续由各自的存储管理。

**接续选定群的文本备注。** 在同一份会话报告中，有旧备注的群可以单独核对。先为该群授权回复及历史读取，核对切换前置条件并暂停连接。点击「核对旧群备注」查看文字，确认实际微信身份相同后提交。新站已有备注时以新站为准。如果旧备注要求默认总结分享，且该群已有附件读取授权，可在接续时勾选「同时开启此群的视频号自动总结」。选项默认关闭，只对确认之后收到的视频号分享生效。接续后可在「设置回复范围」修改备注及自动总结，重复确认不会恢复已清空的备注或已关闭的总结。旧角色规则另行核对。

会话预演和确认使用 `GET` 与 `POST /api/apps/wechat/connections/{connection}/legacy-import/memory/{thread}`。确认提交 `digest` 和 `same_identity: true`，并带管理会话的 CSRF 请求头。

群备注预演和确认在上述单会话路径后追加 `/preference`。仅当预演返回 `can_enable_channels_summary: true` 且主人选择同时开启时，提交才额外包含 `enable_channels_summary: true`。此操作可以独立于旧消息和长期记忆接续。

**接续旧群的触发方式和角色设定。** 保存旧版接入摘要，授权目标群的回复范围，并暂停连接。打开「设置回复范围」，在该群下点击「核对旧群触发方式」。确认新旧连接是同一个实际微信账号，再接续提及时回复、所有群聊文字可触发或自主接话的规则，以及已启用的专用前缀、群角色设定和被 @ 时必须回复。自主接话的确认同时授权大肥鱼观察这个群并自行判断是否参与，不扩大到其他群或好友。该入口直接核对旧账号配置，无需 SQLite 副本或历史读取权限。现有自定义回复规则优先。接续后可在「触发方式与角色」里修改或恢复默认，变更只影响之后的消息。撤销该群的回复范围会清除其触发规则。仅靠旧全局自定义前缀的群仍需单独核对。

群回复规则预演和确认使用 `GET` 与 `POST /api/apps/wechat/connections/{connection}/legacy-import/groups/{thread}/rule`。确认提交 `digest` 和 `same_identity: true`。普通策略编辑用可选的 `reply_settings` 对象，省略时沿用当前群规则，传入 `{"mode":"mention"}` 恢复默认。回复范围、附件读取和视频号分享总结分别由原设置管理。

**核对旧版投递回执。** 保存接入摘要后，从同一连接的「从旧版接入」打开「旧版投递回执」，选择「Agent 投递」或「管理页发送」。两份账本可分别分页和筛选待核对，原快照移走后仍可读取。管理页发送只显示已校验快照里的记录，缺少原生数据集绑定，因此不提供原操作核对。Agent 投递的「核对原操作」只查询已核验的同账号、同资料集原生实例。查询不到时继续显示原快照状态，不提交、重试、取消或恢复旧队列。

接口为 `GET /api/apps/wechat/connections/{connection}/legacy-import/receipts`，支持 `origin=agent|management`（默认 `agent`）、`limit=1..200`、`pending=1` 和返回的 `next_after`。切换来源、筛选或快照后从第一页读取。读取单条时追加 `/{key}`，管理页发送仍须指定 `origin=management`。同键的新站任务继续通过当前投递回执入口查询。

**正式启用前的核对。** 保持旧桌面和旧原生服务停止，保留原部署、原数据和一致快照。按本指南先接续所需模型配置、鱼家目录、私人声线、选定会话和群设置，在新站核对这些资料及工具的可用状态。然后在同一微信连接点击「核对启用条件」。预演检查旧宿主停机记录、已保存的旧投递证据、原生档案凭证、目标在线状态、模型配置及回复范围。Agent 投递与管理页发送的待核对数分别展示，同一次操作可能在两份账本中都有记录。主人确认实际登录身份相同，并确认待核对旧回执继续留在归档后，可以保存启用确认。保存后另行点击「启动 Agent」。接口为 `GET` 与 `POST /api/apps/wechat/connections/{connection}/legacy-import/activation`。

出现停机记录、原生资料集、档案凭证或旧快照不一致时停止切换，先核对原实例与旧投递记录。正式确认需要支持互斥交接的旧 Windows 桌面写出第 2 版停机记录。旧记录仍可用于只读核对。新站确认时在旧数据根保存交接记录。从那时起，支持互斥交接的旧桌面将拒绝启动该目录，即使新站暂停或重启。已保存确认后可以拆离快照，旧数据根的停机与交接记录仍参与新站运行检查。其他旧版本、手动启动的外部进程和其他设备仍需主人核对。

导入档案后先保存原生导入凭证。接续启用前，已登记导入凭证或接续记录的连接只能核对资料、登录和管理已有投递，不能提交新的外发、续发、附件上传、群变更或撤回。启用后每次提交都会重查凭证。查看原操作、获取入站附件及取消已有媒体任务仍可使用。

需要准备回退时，展开该连接的「冻结前投递核对」，先查看状态，再确认冻结。冻结会写入新站并在重启后保留。它停止该连接的新提交和新的续发请求，等待本机工作器和已经放行的原生 HTTP 请求退出。查询回执和取消原媒体任务仍可使用。其他微信连接各自运行。

界面显示「新投递已冻结」后，按页查看已接受、已结束、尚未提交、仍在处理和结果待核对。已接受的分段及服务器回执继续保留。原生端不可用、原操作查不到、身份不匹配或查询超时时，结果保留待核对。分页游标绑定本地检查点和停止记录。本地记录变化时，从第一页重新核对。每页的微信状态以该次查询为准。接口为 `GET` 与 `POST /api/apps/wechat/connections/{connection}/legacy-import/rollback`，提交空对象 `{}` 需要本机主人会话和 `X-CSRF-Token`，读页可传 `limit=1..200` 及上一页返回的 `after`。

排空后点击「保存本次核对」，把当前工作器检查点中的投递和该连接的管理操作一起保存到新站私有数据目录。界面显示证据摘要及仍在处理、待核对的数量。`GET /api/apps/wechat/connections/{connection}/legacy-import/rollback/audit` 查看摘要，向同一路径 `POST {}` 重新采集。先前证据继续可按 `GET .../rollback/audit?digest=<摘要>` 核对。原生端离线或超时时，保存的结果保留未确认状态。保存后的本地记录发生变化，摘要会提示重新采集。

Windows 桌面在启动站和原生端之前取得数据根运行锁，并写入本次启动标识。两项自有服务都正常退出后才写入对应的停机记录。在线保存核对时会关联当前运行标识，解除时用来核对同一次正常停机。已经产生新站写入时，回退须保留新数据及回执。

**离线解除交接。** 先冻结连接并在线保存核对报告，再正常停止成长站。旧桌面也须处于停止状态。在同版成长站的安装目录打开 PowerShell，执行以下命令，将示例路径、连接标识和摘要换成实际值。

```powershell
.\agent-home-station-server.exe legacy-wechat-release --data 'C:\Station\data' --connection '<连接标识>' --audit-digest '<核对摘要>'
.\agent-home-station-server.exe legacy-wechat-release --data 'C:\Station\data' --connection '<连接标识>' --audit-digest '<核对摘要>' --apply '<预演返回的 apply_digest>'
```

预演只读取资料。命令会核对同一连接、同次正常停机、原生数据集、登录导入凭证、本地投递和群操作记录，以及旧桌面原交接凭证。有在途任务时，回到新站核对至终态，重新保存报告并正常停机。有结果未知的条目时，先核对报告。确实接受这些未知结果时，在预演和执行命令中都追加 `--accept-unknown`。它不会放行在途任务，也不会重发或撤回已有消息。

执行先禁用该数据根的成长站原生微信运行，再把旧交接凭证原样归档。返回 `released` 后，主人可以手动启动支持交接的旧桌面。若返回 `resume_required`，以相同核对摘要、参数和令牌续办。成长站该数据根内的其他微信连接共用原生进程，也随之停用。关闭成长站的原生微信启动选项后，仍可使用本地聊天和其他应用。保留两边数据目录、回执和交接文件。此互斥流程适用于支持交接的 Windows 桌面入口，手动启动的外部原生进程和其他设备需要单独核对。

### Windows 切换与回退演练清单

先在隔离目录完成演练。自动演练用临时目录运行新旧 Windows 桌面运行器与 Go 服务，原生微信接口由仅监听本机的合成程序提供。它使用已有合成回执检查归属与保留。演练从没有鱼身份的旧配置导入 Flash 模型设置，首次进入新站再初始化本机身份、聊天原文与成品，在接入及解除后核对同一份记录。未配置模型密钥时只保存聊天原文。全程无需登录微信、提交消息或调用模型。在项目目录中使用空间充足的磁盘运行，构建输出和临时目录均可迁到该盘。

```powershell
$preview = 'E:\AgentHomePreview'
$env:TEMP = Join-Path $preview 'temp'
$env:TMP = $env:TEMP
$env:GOTMPDIR = $env:TEMP
New-Item -ItemType Directory -Force $env:TEMP | Out-Null
.\scripts\Build-Desktop.ps1 -LocalStation -WithWeChatApp -Test -OutputDirectory "$preview\package"
.\scripts\Test-StationWechatCutover.ps1 -PackageDirectory "$preview\package"
```

演练只用合成账号、快照和原生资料。手动复查时也使用隔离副本，确认以下顺序均可复现。

1. 使用支持互斥交接的旧 Windows 桌面正常停止，核对旧数据根的 `legacy-cutover-state.json` 为第 2 版 `stopped` 记录。以该记录、同一账号 ID 和一致快照创建新站接入来源。旧根、快照、新站 `data` 目录和鱼家工作区必须是四个彼此独立的目录。
2. 在新站预演并接入。核对来源账号 ID、连接 ID、旧原生资料集、原生导入档案、会话来源与本地检查点。只对经过授权的会话接续记忆、群备注和规则。接入、预演和启用前任一摘要或快照变化，均从预演重新开始。
3. 新站运行时核对投递的已接受、已结束或待核对状态。隔离演练使用合成投递记录。旧桌面在交接声明存在时应拒绝启动。演练中不得用手动原生进程绕开此检查。
4. 回退时，先在新站冻结连接，等待新提交和续发停止。保存投递与群操作核对报告，记录报告摘要、检查点、原生资料集和未知项目数。未知项目只在明确接受其后果后使用 `--accept-unknown`，仍在处理的项目不能解除。
5. 正常停止新站桌面，核对 `station-native-stop.json` 与报告的桌面 epoch、原生资料集和所有权代次匹配。再运行离线解除预演，保存返回的 `apply_digest`，并以同一参数执行解除。若停机记录、报告、本地检查点或旧凭证发生变化，令牌失效，须重新核对。
6. 获得 `released` 后，确认新站原生微信启动被禁用、旧交接凭证已归档，再手动启动支持交接的旧桌面。核对旧端能访问自己的原数据和新站留下的回执仍完整。新站数据、导入资料和新站投递记录始终保留，不用旧快照覆盖。
7. 关闭成长站的原生微信启动选项，以同一数据根启动。核对主人和大肥鱼身份、家目录授权、模型设置、此前的聊天原文及成品下载。确认微信路由和原生子进程均未启动，抖音等独立应用仍按原方式登记，停机记录与解除凭证仍可核对。

正式环境仅在主人明确授权后沿用同一清单，并额外保留切换前后的只读副本、实际连接 ID、报告摘要、两端停机记录和最终平台回执。任一步无法核对时停止在当前端，保留目录和证据，不自动重启另一端，也不自动提交、重试、撤回或删除消息。

### 准备旧微信启动器更新与恢复

旧端服务停止后，可以先用成长站服务程序核对磁盘上的待办和投递。该命令只输出计数与状态，适用于尚未生成新版停机凭证的旧端。

```powershell
.\agent-home-station-server.exe legacy-wechat-snapshot --audit --legacy-data 'C:\Legacy\data' --account '<旧主账号 ID>'
```

`worker_delivery` 按接续时采用的规则统计文本和媒体回执，`worker_delivery_open` 单独统计旧队列中尚未终结的投递。两项口径不同，旧任务可已经结束但回执仍待核对。`waiting_wakes`、`inbox_pending`、`plan_pending` 和 `media_reply_pending` 用于核对普通启动可能恢复的工作。`admin_delivery` 是管理端独立账本，可能与其他记录指向同一平台操作。`stop_marker_present` 只说明文件存在，正常停机仍由快照预演验证。若本地仍有待唤醒任务、管理端未核对投递或原生队列状态未知，先准备受控交接方式，再启动旧端。

新版 Windows 原生程序提供给桌面启动器使用的 `cutover-hold`。它持有与正常原生服务相同的数据锁，通过私有管道返回进程编号、数据集标识及文本和媒体队列计数，再等待启动器发出停止指令。它只读取已有的注册和控制库。遇到 SQLite 伴随日志、非本机固定卷、重解析点或账号不匹配时拒绝运行。可在合成数据上用 `Test-LegacyWechatNativeHold.ps1 -NativeExe <候选 wx.exe 绝对路径>` 核对该入口。它目前用于验证原生侧的排他占用，正式快照仍依赖完整的桌面交接流程。

使用已构建的兼容旧启动器和带 `cutover-hold` 的原生程序时，在两端服务关闭后从 PowerShell 执行受控停机命令。部署清单中的 `native_sha256` 必须与实际使用的 `wx.exe` 一致。

```powershell
& 'C:\Legacy\WechatAgent.exe' --prepare-legacy-cutover --workspace 'C:\Legacy' | ConvertFrom-Json
```

命令取得桌面互斥和旧数据锁后，只启动持有原生锁的离线子进程。排队或结果未定的原生投递计数非零时停止流程，原记录保留。成功返回 `stopped` 和 `offline_hold`，可直接进入快照预演。旧 Agent 与管理端账本中原有的未知回执仍按原操作核对。若正常启动旧端，启动器会使本次停机凭证失效，快照前须重新核对。隔离演练使用 `Test-LegacyWechatControlledCutover.ps1`，需要旧启动器、原生程序和成长站服务程序候选。

需要配套更新时，常规部署继续使用原有快捷方式、管理端和原生程序。脚本将离线原生程序及其同目录 CRT 发布到 `cutover-native-<候选启动器 SHA-256>`，核对清单后原子替换旧桌面入口 `WechatAgent.exe`。只有受控停机命令会读取当前启动器对应的配套目录。用当前源码构建候选，使用不同于正式安装的构建目录。

```powershell
.\scripts\Build-Desktop.ps1 -Test -OutputDirectory 'C:\Temp\ahs-legacy-candidate'
.\scripts\Build-WindowsNative.ps1 -OutputDirectory 'C:\Temp\ahs-legacy-native-candidate'
$candidate = 'C:\Temp\ahs-legacy-candidate\WechatAgent.exe'
$native = 'C:\Temp\ahs-legacy-native-candidate\wx.exe'
$swap = @{
    Workspace = 'E:\A-Importance\wechat-bot'
    Launcher = 'E:\A-Importance\wechat-bot\dist\windows-native\WechatAgent.exe'
    Replacement = $candidate
    ReplacementSha256 = (Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash
    NativeReplacement = $native
    NativeReplacementSha256 = (Get-FileHash -LiteralPath $native -Algorithm SHA256).Hash
}
$preview = .\scripts\Update-LegacyWechatLauncher.ps1 @swap | ConvertFrom-Json
```

预演核对互斥锁、旧服务、配置端口、运行清单和交接声明，返回配套目录与摘要。实际更新须先正常停止旧桌面，按原参数追加 `-ApplyDigest $preview.apply_digest`。`updated` 返回原程序的 `rollback_file` 和 `previous_sha256`。若文件、配置或停机记录发生变化，重新预演。若中断发生在替换启动器之前，重试时可复用逐文件复验通过的配套包。更新成功后，以上述受控命令取得停机凭证。仅更新兼容启动器时，可省略两个原生候选参数，之后沿用与所装原生版本兼容的停机方式。

恢复时用原 `Workspace`、`Launcher`，将 `Replacement` 和 `ReplacementSha256` 改为更新结果的 `rollback_file` 与 `previous_sha256`，并移除两个原生候选参数。加上 `-Restore` 先预演，再以相同参数追加本次摘要执行。已有有效交接声明时，先走前面的离线解除流程。恢复后当前配套目录失效，文件留存供核对。恢复或配套更新会归档已有停机凭证，`invalidated_stop_proof` 给出本机归档路径。若 Windows 无法确认替换结果，按错误中列出的路径先核对启动器、副本和暂存文件，停机凭证以实际运行记录重新建立。

### 制作旧微信停机快照

安排维护前可用 `legacy-wechat-snapshot --check-source --legacy-data <旧数据绝对目录> --snapshot <预定快照绝对目录> --account <主账号>` 核对来源和目标。命令检查账号规则、账本和 SQLite 记忆，二次读取核对来源内容，仅返回计划文件、账号、记忆库和投递记录的数量以及预计文件总字节数。它无需停机凭证，也不生成快照、锁文件或执行令牌。原生队列、停机凭证和实际发布权限仍须在正式维护窗口核对。

先用支持互斥交接的旧桌面取得正常或受控停机凭证，再用同版成长站的 `agent-home-station-server.exe` 预演。旧数据目录和快照目录应位于本机磁盘且彼此独立。快照父目录需已存在，目标目录需尚未创建，且应放在源码库与鱼家工作区之外。

```powershell
.\agent-home-station-server.exe legacy-wechat-snapshot --legacy-data 'C:\Legacy\data' --snapshot 'C:\PrivateMigration\wechat' --account '<旧主账号 ID>'
```

预演会核对旧桌面的停机锁、第 2 版正常停机凭证、在册账号、投递账本和 SQLite 记忆，只返回计数与摘要。核对结果后，用原参数和返回的 `apply_digest` 制作快照。

```powershell
.\agent-home-station-server.exe legacy-wechat-snapshot --legacy-data 'C:\Legacy\data' --snapshot 'C:\PrivateMigration\wechat' --account '<旧主账号 ID>' --apply '<预演返回的 apply_digest>'
.\agent-home-station-server.exe legacy-wechat-snapshot --snapshot 'C:\PrivateMigration\wechat' --verify
```

制作时会先写入当前 Windows 用户与系统可访问的私有暂存目录，复验文件清单、SHA-256 和 SQLite，再原子发布。源内容变化、目标已存在、SQLite 仍有伴随文件或无法通过只读校验时停止。中断后如目标已存在，先用 `--verify` 查明结果。独立校验通过后，把目录和返回的 `manifest_digest` 登记到[旧版微信接入](#在隔离副本预演旧版微信接入)配置。

新站在预演、确认接入、启用核对以及接续记忆和群设置时自动复核新快照的文件清单和摘要。配置固定了清单摘要后，清单丢失也会拒绝接入。采用新格式的连接在保存归档时会记录该摘要，后续接续使用同一份来源。需要调整源资料时，请重新制作和核对快照。已导入的历史回执仍可从新站归档查看。清单用于本机文件完整性核对，未做数字签名。

快照保留已登记账号的微信回复规则、Agent 投递账本、管理页发送回执和支持的记忆库。管理页回执从活跃账本及经核对的历史双索引提取，只保存发送身份、结果及分段回执。`management_delivery_record_count` 单独计数。同一平台操作可能同时出现在两份账本中。账本和记忆库可能包含原始私聊，须按宿主私有资料保存。模型凭据、管理口令、微信登录档案、声线和素材继续使用各自的独立接续流程。快照不会将旧任务加入新站的可执行队列。

### 接续仍有待写回记录的原生数据

官方格式的 `store export` 与原生状态迁移有不同用途。官方格式导出不包含原生端的加密登录状态、历史发送账本和未完成同步投影。Windows 原生端的 `cutover-snapshot` 可以将已停机的受管原生数据完整接续到新站，保留原数据集编号、加密恢复状态及待写回记录。它不修复投影，也不启动网络服务。目标的 API 令牌由新站启动时单独创建。

先使用兼容旧启动器取得停机凭证。新站 `data/station/apps/wechat` 父目录应属于当前用户，且仅向当前用户和 SYSTEM 开放，`native` 目标必须不存在。两端程序均保持停止，运行已核验的原生程序。

```powershell
& $newWx --json --data-dir $stoppedNativeRoot --account $account `
  cutover-snapshot --destination $stationNativeRoot --source-stopped
```

命令取得旧原生锁，检查所有账号的排队与结果未定投递、数据库完整性、SQLite 伴随文件和目录链接。账号档案及密钥文件必须位于各自受管账号目录。复制后只在目标注册表更新这两类绝对路径，源文件和账号状态保持一致。目标已有数据时拒绝覆盖，重复执行应先核对第一次的返回和实际目录。返回 `nativeStatePreserved: true` 表示原生恢复状态已接续，`profileProjection: preserved_without_repair` 表示仍按原记录保留投影进度。微信是否在线及历史是否完整继续通过新站实际接口核对。

这一操作保留原生发送账本。只有旧 Agent 管理数据快照通过核对、接入归档已保存且主人确认会话授权后，才可以启动新站的自动回复。旧 Agent 队列仍使用独立归档流程。

### 文件与自编工具

模型可按需发现 `server_list/read/search/write/edit`，在上述家目录内读取、搜索、新建和修改文件。相对路径以家目录为根，绝对路径也必须在家目录内。新建不会覆盖已有文件；修改需要先读取，再提交内容哈希和要替换的唯一片段。文件与 `Project Tools` 下的自编工具跨重启保留。

`server_windows_process` 提供启动、列表、状态、输出和取消。启动使用绝对可执行文件路径、参数数组、家目录内工作目录及稳定的 `request_key`；同编号不会重复启动。进程完成与聊天回复是两种状态，目前要由模型在本轮查询，或由后续请求按原编号续查，尚未接入完成后主动唤醒模型的调度器。重启时未确认完成的进程标为中断，不盲目重放。

暂停工作空间会拒绝新的文件操作和程序启动，并取消仍在运行的程序；已有进程记录仍可查询和取消。写文件与暂停共用身份锁，已开始的一次原子文件操作可以先完成。运行程序的撤权检查约每秒一次，不表示暂停能撤回它此前已经写好的文件。

### 项目工具与本机成品

需要文档工坊的 Windows 预览可在源码目录运行以下构建命令。该预览可以独立启动，不需要先接入应用。

```powershell
./scripts/Build-Desktop.ps1 -LocalStation -WithDocumentWorkshop -Test -OutputDirectory ./temp/local-station-document
```

初始化本机主人后，打开「项目工具」，确认文档工坊的本机素材权限，再安装、启用。然后在「聊天」里请大肥鱼把一段文字或表格做成可编辑 Word 文档。工具实际产出并通过校验后，大肥鱼会提供成品编号，聊天中可直接下载。停用文档工坊后，已登记且来源仍可用的成品仍能下载。未安装或停用时，本地聊天仍可照常使用。

文档工坊会在第一次有制作任务时启动，空闲约 5 分钟后退出进程。检查和导出已有 DOCX 时，工具按任务运行，只读挂载该次获准的成品。未完成的回复重试时会先核对原任务。生成和登记均在本机，聊天推理由独立配置的模型完成。

在聊天中可用已有的 `work-…` 成品编号请大肥鱼检查 DOCX，或转换为 PDF 后给出下载链接。DOCX 正文和版式按位置续读。原 DOCX 仍可下载。PDF 记录输入成品编号和校验值，输入来源被遗忘时派生 PDF 也会失去访问权限。

PDF 导出还需可用的文档运行时。构建时为上述命令同时提供 `-DocumentPythonPackagesDirectory` 与 `-DocumentFontsDirectory`，分别指向经过审核的纯 Python 依赖和可分发字体目录。「项目工具」会显示 PDF 导出是否就绪。已有旧版文档工坊时，先停用，再确认原有本机素材权限并更新，然后重新启用。当前本地文档入口覆盖已登记 DOCX 的检查、生成和基础版式导出。OCR 与 PDF 页面视觉比对仍需独立接入和验证。

已有相同固定版本的文档环境时，可以复用其中的依赖与字体，不必重新下载或启动 Office。准备脚本接受已经核对为审核摘要的 ReportLab 离线补丁；重复运行会检查现有清单及全部已登记文件的哈希，损坏或路径越界时停止，不覆盖现有目录。只需 Word 与 PDF 的场景可省略 OCR 和页面渲染依赖。

```powershell
python -B scripts/prepare_document_runtime.py --target '<新版目录>/runtimes/document-workshop' --python-packages-source '<旧版目录>/runtimes/document-workshop/python-packages' --fonts-source '<旧版目录>/runtimes/document-workshop/fonts'
```

Windows 工具启动器会在权限、来源和文件校验完成后，将进程使用的路径转换为扩展格式。这样鱼身份、工具摘要和任务目录组成的较长路径也能启动工具。路径格式转换不增加文件授权。可用 `AHS_LOCAL_PROJECT_TOOL_RUNTIME_ROOT=<新版目录>/runtimes` 与 `AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST=1` 运行 `go test ./internal/station -run '^TestLiveLocalProjectToolCreatesImmutableDocumentThroughChat$' -v -count=1`，通过合成聊天模型和实际隔离进程核对中文可编辑 Word、PDF、下载、重启及来源失效。该测试不访问模型提供商和微信。

### 音频工坊

用 `./scripts/Build-Desktop.ps1 -LocalStation -WithAudioWorkshop -Test -OutputDirectory ./temp/local-station-audio` 构建 Windows 音频预览，也可以与 `-WithDocumentWorkshop` 一起使用。音频工坊使用项目内受管 Python，独立于微信、GPU 和在线服务。

在「项目工具」确认本机素材读写授权，安装并启用「音频工坊」。把大肥鱼家目录内的 PCM WAV 登记为成品后，可在本地聊天说「把这个成品从第 12 秒开始截取 8 秒，转成单声道」。聊天收到新成品编号后可直接下载。结果报告实际起点、时长和是否读到末尾。工具保留原采样率，输出 16 位 PCM WAV，输入及输出每份最大 512 MiB。长音频可指定起点与时长分段处理。

### 独立 RVC 换声

用 `./scripts/Build-Desktop.ps1 -LocalStation -WithRVCBackend -Test -OutputDirectory ./temp/local-station-rvc` 构建预览。它包含项目工具声明、独立控制脚本及受管 Python。推理依赖、上游 RVC、分离模型缓存、声线权重和索引需由主人按许可准备。文件和模型留在本机。

在传给本地服务的 `-data` 目录中创建 `station-rvc.json`。以下是示例，目录须替换成已核验的本机资源。回环端口应留给本机 RVC。

```json
{
  "schema_version": 1,
  "enabled": true,
  "workspace": "D:\\StationRVC\\resources",
  "media": "D:\\StationRVC\\windows-media",
  "port": 6198,
  "profiles": [{"id": "private-example", "label": "本机试听声线"}]
}
```

`workspace` 与宿主数据、大肥鱼可编辑目录分开，包含 `public_workspace` 和 `pri_workspace`。前者提供已核验的 Windows 运行时、RVC 项目及适用的公共模型；`media` 提供 `ffmpeg.exe` 和 `ffprobe.exe`。私人声线登记在 `pri_workspace/<fish_id 的 SHA-256>/voice-library/manifests/<声线 ID>.json`，清单指向本私人声线库内的配置。声线配置记录模型和索引的实际 SHA-256，并绑定所属运行时。现有微信私有声线需要单独核准后登记，保留原模型和原配置。回环令牌由宿主管理。

打开「项目工具 → RVC 换声」，确认任务素材读写与本机 GPU 权限后安装、启用。把一份 PCM16 WAV 登记为本机成品，在聊天中指定该成品和声线，例如「把这个成品用本机试听声线换声」。单段最多 600 秒及 25 MiB。需要先截取时可用音频工坊。

提交后聊天会获得任务编号。通过同一编号可查进度、取消或明确续办。完成的音频经源文件、声线、实际设备、时长和摘要核对后登记为可下载成品。提交回执未知时，宿主用原请求键查询。重启后保留原任务及进度，续办沿用原素材和原声线。

`python -B scripts/test_station_rvc_bridge.py` 可复跑合成桥接验收。指定 `AHS_STATION_RVC_CONTROL_ROOT` 可指向已打包控制脚本。真实模型推理和试听另行验收。

#### 接续旧版私人 RVC 声线

完成鱼身份接续后，可以用以下主人私有计划预演旧版唱歌声线的接续。适用于新站首次登记 RVC，`deployment` 复用上面的运行环境登记，`profiles` 留空。旧版配置里的解释器、RVC 项目和分离运行环境须在该登记的公共运行环境范围内。

```json
{
  "schema_version": 1,
  "library": "E:\\LegacyData\\account-private\\voice-library",
  "source_owner_id": "旧条目登记的主人 ID",
  "voice_ids": ["legacy-rvc-example"],
  "deployment": {
    "schema_version": 1,
    "enabled": true,
    "workspace": "E:\\LegacyData\\managed-workspaces",
    "media": "E:\\StationRuntime\\windows-media",
    "port": 6198,
    "profiles": []
  }
}
```

`library` 指向同时包含 `catalog.json` 和私人配置清单的旧库。`voice_ids` 填旧目录里的声线编号。若条目从未登记主人，确认有权使用后删除 `source_owner_id` 并填写 `"allow_unowned": true`。预演会核对私人配置的引用、运行依赖、模型和索引的 SHA-256，并给出选定的两个编号及复制字节数。旧目录展示的配置路径与实际运行清单不同时，预演用 `catalog_path_differences` 提醒核对，接续按旧桥接器实际读取的运行清单处理。模型和索引预算默认 4 GiB，可在计划中用 `max_bytes` 按实际磁盘空间调整。

```powershell
$stationServer = 'E:\AgentHomePreview\agent-home-station-server.exe'
$stationData = 'E:\AgentHomePreview\data\station'
$rvcPlan = 'E:\PrivateMigration\rvc-plan.json'
$preview = & $stationServer legacy-rvc --data $stationData --plan $rvcPlan
if ($LASTEXITCODE -ne 0) { throw 'RVC 预演失败' }
$digest = ($preview | ConvertFrom-Json).digest
# 核对声线和所需空间后使用原计划及摘要
& $stationServer legacy-rvc --data $stationData --plan $rvcPlan --digest $digest
```

自定义鱼家工作空间时，在两个命令里都传入同一个 `--workspace` 绝对路径。确认后，所选模型和索引复制到该鱼身份的私有声线库，原路径仍保留原件。接续同时登记旧声线编号、原换声配置编号和有效推理参数。资料已写入而运行登记中断时，可用同一摘要续办。遇到其他已有 RVC 配置或私有声线库时，先核对现有资料。接续后仍在「项目工具 → RVC 换声」安装并启用，由工具列出可选择的编号。

### Windows ComfyUI 创作

`Build-Desktop.ps1 -LocalStation -WithComfyUI` 打包项目工具声明。准备一份主人核验过的 Windows 便携 ComfyUI 0.37.2。运行时、模型和工作流目录由主人管理，须位于大肥鱼家目录和站点数据目录之外。可以复用现有安装的文件，站点启动自己的回环进程。选一个空闲端口，避免与原 ComfyUI 或 YuE 冲突。

在「项目工具 → ComfyUI 创作」的大弹窗中填写安装目录、模型目录、已批准工作流目录及运行文件审核清单。常见便携包的 Python、入口及前端相对路径已有初值，不同布局可修改；端口、CPU 或 CUDA、GPU 索引和闲置时间在同一表单调整。确认运行文件和工作流已经审核后保存，宿主会核对文件摘要及工作流，不启动程序，也不会自动安装或启用工具。保存后再按工具页面确认安装与计算权限并启用。

修改已配置的后端前先停用工具，并处理仍待核对的原任务。版本冲突或校验失败保留页面草稿和原登记。已有手写登记继续兼容，下面是对应宿主文件 `station-comfyui.json` 的格式，用于离线部署或核对，不要求日常手写。

闲置时间按秒设置，`0` 沿用默认五分钟；它不表示处理完成后立刻退出。显式数值按原值校验，不会自动改成默认值。超过计时器可表示范围的值会明确拒绝。

```json
{
  "schema_version": 1,
  "enabled": true,
  "runtime": "D:\\StationComfy\\portable",
  "python": "python_embeded\\python.exe",
  "entry": "ComfyUI\\main.py",
  "frontend": "python_embeded\\Lib\\site-packages\\comfyui_frontend_package\\static",
  "manifest": "D:\\StationData\\comfyui-manifest.json",
  "models": "D:\\StationComfy\\models",
  "workflows": "D:\\StationComfy\\approved-workflows",
  "port": 21984,
  "idle_seconds": 300,
  "device": "cpu"
}
```

同一站点 `-data` 目录内的 `comfyui-manifest.json` 使用 `schema_version: 1`、`policy: "station-comfyui-v1"`，以及 `files` 对象。对象键为运行时内相对路径，值为对应文件 SHA-256 的小写十六进制。至少固定上述 Python 和 `main.py`，也可登记 `server.py` 等已核验文件。可用 PowerShell 的 `Get-FileHash -Algorithm SHA256` 取得摘要。该清单校验列出的文件，整个安装及模型仍应来自主人信任的来源。

把主人批准的 API 格式工作流放到工作流目录。例如 `smoke.json` 使用一个 `EmptyImage` 节点和一个 `SaveImage` 节点。创建同目录的 `registry.json`，把示例摘要替换为原图文件的实际摘要。

```json
{
  "schema_version": 1,
  "comfyui_version": "0.37.2",
  "workflows": [{
    "id": "smoke",
    "label": "离线图像样例",
    "graph": "smoke.json",
    "sha256": "替换为工作流文件的 64 位小写 SHA-256",
    "save_node": "9",
    "parameters": [{
      "id": "width", "label": "宽度", "kind": "integer",
      "minimum": 16, "maximum": 512, "node": "1", "input": "width"
    }]
  }]
}
```

打开「项目工具 → ComfyUI 创作」，确认本机成品写入与 GPU 权限，安装并启用。可在聊天中说「运行离线图像样例，宽度 48，完成后保存 PNG」，之后可按原任务编号查询或取消。每个任务最多登记 16 张 PNG，参数越界时会提示。当前支持内置节点，按进程参数关闭自定义节点、API 节点及图像提示元数据。CPU 与 CUDA 由主人在部署登记中选择，CUDA 可指定 `cuda_device`。

回执不明时只查原任务，原进程结束后 ComfyUI 的内存队列可能无法查回。此时宿主保留待核对任务记录，新的生成需要新的请求。已登记的成品可以继续下载。独立进程按需启动，处理及核对期间持有运行租约。正式换用 GPU 前需按本机安装单独验收。

### Windows YuE 作曲

构建时使用 `Build-Desktop.ps1 -LocalStation -WithYuE` 暂存官方项目工具。构建包内含工具声明。便携运行时、模型、FFmpeg 和工作流由主人准备，并存放在家目录和站点数据目录之外。YuE 使用单独的本机端口和进程，可和 ComfyUI 图像工具分别启停。

打开「项目工具 → YuE 作曲」，在工具详情填写已经安装的运行目录、模型目录、工作流目录和 FFmpeg 文件。保存按钮固定在弹窗顶部；核对运行文件和工作流后勾选确认，再保存部署。

- 当前运行器使用 ComfyUI 0.37.2；运行细节中的 Python、入口和前端路径均相对于安装目录。常见便携布局已提供默认值，可按实际安装修改。
- 工作流目录应含 `audio_yue2_text_to_music.json` 和 `audio_yue2_music_cover.json`。宿主核对结构并计算摘要；后续执行仍会根据后端实际能力检查参数。
- 审核清单选填，留空时由宿主生成私有版本清单，记录当前接入策略要求的 Python 和入口摘要。已有的明确清单及摘要继续严格核对。
- 回环端口默认 8191；闲置停止设为 0 时沿用默认 20 分钟。选择 CUDA 时可指定 GPU 编号。保存配置后，工具仍需单独安装并启用，生成任务到来时才启动计算进程。

已有 `station-yue.json` 会回显到同一界面，无需重新登记。修改运行环境前先停用工具并核对未完成任务；冲突或保存失败时保留原登记和当前草稿。部署检查通过后，还需分别验证实际权重、GPU 推理和音乐质量。

在「项目工具 → YuE 作曲」确认权限、安装并启用。原创任务使用风格、歌词和时长。参考音频任务使用已核验的 PCM16 WAV 成品编号，单份参考文件最多 25 MiB，可先用音频工坊转换。任务账本保存生效配方、参考来源、工作流摘要和原任务编号。提交结果未知时按原任务核对，取消先写入账本，迟到结果不会登记为成品。只有完整解码并通过时长核验的 FLAC 才进入可下载成品。指定某条唱歌声线需要另行运行已获授权的 RVC 任务。

### Windows 本地语音合成

使用 `Build-Desktop.ps1 -LocalStation -WithStationSpeech` 构建带「本机语音」项目工具的 Windows 预览。该选项准备隐藏执行所需的受管 Python CLI，并提供可登记的 `runtimes/windows-station-speech/gpt_sovits.py`。主人另行准备 GPT-SoVITS 项目、推理用 Python 及其依赖、基础模型、解码组件和获准的说话声线。把部署登记 `station-speech.json` 放在站点 `-data` 私有目录。资源路径使用绝对路径，运行环境与私有声线文件放在站点数据和大肥鱼家目录之外。

下面是登记示例，使用实际路径和每个文件的 64 位小写 SHA-256。`media` 目录须含 `ffmpeg.exe` 与 `ffprobe.exe`，`nltk_data` 指向准备好的本地语料目录。有独立 Python 依赖目录时，可用 `packages` 登记其绝对路径。当前运行器使用 GPT-SoVITS v2 声线。

```json
{
  "schema_version": 1,
  "enabled": true,
  "project": "E:\\Speech\\GPT-SoVITS",
  "python": "E:\\Speech\\Python\\python.exe",
  "base_models": "E:\\Speech\\BaseModels",
  "runner": "E:\\AgentHomePreview\\runtimes\\windows-station-speech\\gpt_sovits.py",
  "runner_sha256": "填写实际 SHA-256",
  "media": "E:\\Speech\\Media",
  "nltk_data": "E:\\Speech\\NLTK",
  "device": "cpu",
  "voices": [{
    "id": "voice-example",
    "label": "示例声线",
    "gpt": "E:\\PrivateVoices\\Example\\voice.ckpt",
    "gpt_sha256": "填写实际 SHA-256",
    "sovits": "E:\\PrivateVoices\\Example\\voice.pth",
    "sovits_sha256": "填写实际 SHA-256",
    "reference": "E:\\PrivateVoices\\Example\\sample.wav",
    "reference_sha256": "填写实际 SHA-256",
    "prompt": "E:\\PrivateVoices\\Example\\sample.txt",
    "prompt_sha256": "填写实际 SHA-256",
    "prompt_language": "all_zh",
    "text_language": "auto"
  }]
}
```

每条声线使用稳定编号，绑定 GPT 权重、SoVITS 权重、参考音频和参考转写的实际 SHA-256，并设置参考语言与合成语言。可用 `Get-FileHash -Algorithm SHA256` 核对文件。复用原有说话声线时，填入原权重、参考音频及同一条声线编号。独立登记不会改动原微信声线。模型与录音由主人保留，工具只向本地聊天返回声线编号、名称、版本和语速范围。

在「项目工具 → 本机语音」确认本机计算与成品写入权限，安装并启用。之后可以在软件聊天中说「用我的说话声线慢读 cloth 和 fabric，给我音频」。大肥鱼可先查询精确声线编号，再合成正文。`speed` 默认 `1`，`0.8` 表示将调速参数设为正常的 0.8 倍，实际允许范围由所选声线给出。`text_language` 留空时采用该声线的登记值。单次正文上限为 12,000 UTF-8 字节，执行时间还受本地聊天回合剩余时间约束。长文本可分为多个请求。

合成按请求启动一次隐藏的离线隔离进程，停用工具或取消聊天请求会中断执行。通过声线版本、WAV 文件和任务回执核验后，成品可在软件聊天中下载。提交后进程意外中断、结果无法核对时，原请求保留待核对记录。工具不会替换声线，也不向任何应用发消息。

本机语音工具包 0.1.1 增加原任务查询与取消。已有 0.1.0 安装时，可在「项目工具」先停用，再确认更新并启用。请在聊天中提供原任务编号以查询或取消。执行器意外退出后，查询只核对已登记的结果；要重新合成，需从聊天发起新请求。取消后的旧任务仍保留状态。

### 接续已登记的说话声线

完成鱼身份接续后，可以把旧声线库选中的 GPT-SoVITS 声线登记到新站。准备上节的部署配置，把其中的 `voices` 改为空数组，再作为以下计划的 `deployment` 对象。将完整计划保存到主人管理的私有目录。

```json
{
  "schema_version": 1,
  "library": "E:\\OldStation\\voice-library",
  "source_owner_id": "旧目录登记的主人 ID",
  "voice_ids": ["voice-example"],
  "prompt_language": "all_zh",
  "text_language": "auto",
  "deployment": {}
}
```

`library` 是含 `catalog.json` 的旧库目录。按条目填写准确的声线 ID。有主人字段的条目须与 `source_owner_id` 一致。确认要接续自己有权使用的旧版无主人条目时，删去 `source_owner_id`，填写 `"allow_unowned": true`。命令适用于首次写入新站的语音登记，一次可选同一来源下、使用相同语言设置的多条声线。需要合并多个来源时，主人可按上节格式逐条核对并维护同一份私有语音登记。

```powershell
$stationServer = 'E:\AgentHomePreview\agent-home-station-server.exe'
$stationData = 'E:\AgentHomePreview\data\station'
$planFile = 'E:\PrivateMigration\speech-plan.json'
$previewJSON = & $stationServer legacy-speech --data $stationData --plan $planFile
if ($LASTEXITCODE -ne 0) { throw '声线预演失败' }
$preview = $previewJSON | ConvertFrom-Json
$preview | Select-Object status, fish_id, voices, bytes_verified, digest
# 核对后使用同一个计划和摘要
& $stationServer legacy-speech --data $stationData --plan $planFile --digest $preview.digest
```

桌面使用自定义工作空间时，两条命令都传入同一个 `--workspace` 绝对路径。预演检查选中的权重、参考音频和转写的大小及 SHA-256。有多个同类文件时，须能从旧登记确定唯一推荐项。确认后把声线 ID、原文件路径和版本绑定写入新站私有登记，接续记录留在同一个数据目录。原件继续从旧库读取，需保留在原位置。接续中断可用原摘要重试，已有不同语音配置时请保留现有配置并核对。接续成功表示登记和完整性核验通过，首次使用仍需核对实际合成和听感。唱歌声线沿用独立的 RVC 登记。

### Windows 运行器

可执行文件旁存在下列受管运行时布局时自动识别；没有安装运行器也能聊天及操作文件，启动程序会返回 `windows_runtime_not_configured`，不会退回不受限制的宿主命令行。

```text
runtimes/windows-python-cli/pythonw.exe
runtimes/windows-python-cli/python.exe
runtimes/windows-agent-host/windows_agent_host.pyw
```

开发预览可用 `-runtime-config` 指向绝对路径 JSON：

```json
{
  "pythonw": "E:\\AgentHomePreview\\runtimes\\windows-python-cli\\pythonw.exe",
  "helper": "E:\\AgentHomePreview\\runtimes\\windows-agent-host\\windows_agent_host.pyw"
}
```

运行器配置限 64 KiB。配置、解释器、隔离代理和可选的 `shell` 须使用工作空间外的普通文件，路径不能经过链接。解释器与代理的依赖目录也不能和工作空间重叠。进程控制和暂存使用宿主私有目录下的 `local-execution-spool`。Windows LPAC 仅将鱼家目录作为可写挂载，受管运行时以只读方式挂载。运行器无效或隔离失败时明确报告原因。

旧管理端的 `/local-home` 使用原身份注册表和管理登录，只提供身份与家目录准备，并明确引导进入独立服务聊天。不要同时运行旧版和新版程序写同一份真实身份文件。

新代码读取 v1/v2 身份文件时不自动初始化；写操作升级为 v3并保留原绑定。新版进程在 Windows 通过文件锁串行更新，锁文件不跟随最终重解析点，持锁期间不可替换。旧程序不理解 v3，不能在新记录写入后直接切回旧版继续写入；应停下相关服务，由兼容版本修复。

## 验证与下一步

### 组合接续演练

组合回归在同一个全新站点里接续模型与身份、家目录项目、两类声线、微信旧回执和选定会话记忆，并验证重新打开服务后仍可读取。微信对端使用合成回执和消息，模型调用与实际外发次数为零。

```powershell
go test -race ./internal/station -run 'Test(LegacyCutover|LocalWechatLegacyImportKeepsEvidenceWithoutReplay|LocalCreationTool)' -count=1
python -B scripts/test_station_cutover_cli.py --binary 'E:\AgentHomePreview\agent-home-station-server.exe'
```

第二条命令需要带 `-WithStationSpeech -WithRVCBackend -WithComfyUI -WithYuE` 构建的 Windows 包。脚本生成合成资料，经实际命令行完成接续，再在本机管理接口中启用四个项目工具，重启并核对。它验证部署登记和持久化，真实模型合成、GPU 与听感有各自的验收步骤。

### 独立连接读取聊天历史

应用页展开连接的回复设置，在“历史与成员读取会话 ID”填写已授权回复的会话。保存后重新启动该连接的 Agent，新回合可以发现 `wechat_read` 和 `wechat_message_read`。留空则关闭；默认没有历史权限。API 使用 `PUT /api/apps/wechat/connections/{connection}/policy`，在 `reply_threads` 之外提交 `history_threads`。

`wechat_read(kind=messages|members,search?,cursor?,limit?)` 仅查询当前会话。页大小按请求传给原生服务，不以默认 50 条限制能力；原生服务实际覆盖范围与下一页游标随结果返回。消息页按原时间正序，读取多页后须按原时间合并，不能把分页返回先后当作事件先后。游标只限当前回合、会话和查询，失效从首批重读。

`wechat_message_read(message_id,offset?,length?,expected_sha256?)` 返回实际原文范围、身份、原时间、引用服务器编号和内容摘要。默认读取 4,000 字节，传输单页最多 65,536 字节。更大的请求明确返回实际长度和续读位置。续页须带上一页摘要，文本更改后从头读取。已知来源被遗忘或隔离后，此工具拒绝重新读取。

主人在「应用 → 微信连接 → 通讯录」可搜索该连接已同步到本机的联系人。默认每页 50 条，原生接口单页最多 200 条。需要读取更多条目时使用「下一页」。界面显示本机快照的覆盖情况，查询不会启动远端联系人同步。

如需大肥鱼在软件聊天框中查阅，请在同一连接的回复范围里开启「允许大肥鱼在本地聊天中查阅此通讯录」。旧配置默认关闭。只修改此项不需要重启微信 Agent。工具 `wechat_contacts({})` 返回获准连接的编号，提供 `connection_id` 后可搜索及分页。续读用 `cursor` 传上一次的 `next_cursor`，并保持搜索词不变。应用页的分页凭证与本地聊天的分页凭证分别签发，撤销 Agent 授权后旧聊天凭证失效。

在同一连接打开「群聊」，可以搜索已同步到本机的群。选择群并确认开启或关闭免打扰后，查看该群的原操作回执。页面中的当前设置来自本机快照，提交是否成功由原生回执核对。已提交、待核对和结果未知都会保留，连接暂停时可查旧结果。原生响应丢失时可刷新，通过原请求键查找已有操作。查不到时保持结果未知，原操作核实前不会创建替代提交。

若后台支持群改名，选中群后点击「修改群名称」，输入新名称并点击「确认改名」。通过「刷新群资料」查询原操作。缓存群名可能晚于微信回执更新，页面分别显示两者。

若后台支持修改本人群昵称，在同一群的资料区可设置昵称，也可留空后明确确认清空。这项设置只作用于当前连接的本人。通过「刷新群资料」核对昵称操作，缓存未返回昵称时会显示未知。

若希望大肥鱼代为设置，在连接的回复范围中填写允许大肥鱼设置免打扰的群 ID，然后保存回复范围。每行一个精确群 ID，留空并保存即撤权。这个权限独立于会话回复范围，适用于授权之后发起的本地聊天。可请大肥鱼列出获准的群、开启或关闭指定群的免打扰，以及查询最近一次操作状态。它使用同一条原生回执链。重试原聊天请求不会再次提交，已有结果未知的操作会继续占用该群的提交位置。

### 微信视频与音轨

`wechat_video_read(message_id,max_bytes?,timeout_seconds?)` 需要附件读取权限，较早的消息还需历史读取权限。普通视频通过微信原生附件接口获取。视频号卡片优先使用原生直链，定位或下载失败可调用同一张卡片的分享解析。默认 50 MiB，可按需调整至 512 MiB 的传输上限。工具超时可配置，取零时继承上游取消。视频号下载服务另有 240 秒请求边界。结果报告素材 SHA-256、时长、帧数和尺寸。

需要视频号解析回退时，Windows 预览构建可带 `-WithShareReader` 暂存官方工具源码。运行目录同时需要 `runtimes/windows-python-cli` 和 `runtimes/windows-agent-host`，两者由现有 Windows 运行时准备流程提供。应用页展开当前微信连接的「视频号解析」，安装并授予受控联网权限，配置适用的解析凭据或本机对象桥接，再启用。已启用时先停用再修改配置。插件按需启动，默认空闲 5 分钟回收。已有解析配置需要主人在新连接内单独配置。

需要读画面或音轨时，构建可加 `-WithMediaUnderstanding`，也可与 `-WithShareReader` 同时使用。解码使用 FFmpeg 和 FFprobe，桌面优先使用程序旁 `runtimes/windows-media` 中的组件。展开微信连接的「媒体理解」，安装并授权媒体处理和模型调用。视觉可单独配置 OpenAI 兼容接口。视频音轨的「语音识别方式」可选「远程接口」或「本机识别」。远程接口填写 HTTPS 地址、模型和密钥。本机识别先按站点的 `station-asr.json` 登记资源，再启用项目工具中的「本地媒体识别」，随后为该微信连接选择本机识别。微信只交付获准的音频，本机模型路径和凭据仍由宿主管理。本机识别不可用时返回错误，不会改用远程语音识别接口。

本地媒体识别启用前会检查模型、SDK、解释器和资源登记；缺少文件时不保存新的识别配置。启用后，工具列表也核对资源与授权，资源被移走时显示未就绪及原因。Windows 识别进程对已核验的长路径使用扩展路径格式，兼容安装路径中包含完整鱼身份与工具摘要的情况。

微信连接选择本机识别后，保存和启用媒体理解都会核对这项站点资源。连接详情中的“已启用”表示设置状态，本机识别缺失会单独显示未就绪提示；已保存的视觉设置仍保留。恢复站点资源后刷新详情，再按原配置启用即可。

已有本机 SenseVoice 资源时，可设置 `AHS_LOCAL_ASR_RESOURCES` 为包含 `public_workspace` 的资源目录，`AHS_LOCAL_PROJECT_TOOL_RUNTIME_ROOT` 为候选发行包的 `runtimes`，运行 `go test ./internal/station -run '^TestLiveLocalStationASRThroughWindowsIsolation$' -v -count=1`。该回归在新建的隔离身份下通过管理接口安装、启用工具，检查一秒合成静音确认为无人声，不连接模型提供商或微信。额外设置 `AHS_LOCAL_ASR_SYNTHETIC_WAV` 可以传入本机合成的 16 kHz 单声道 PCM16 英文测试录音，内容包含 “local speech recognition test”，检查识别链路。不要传入真实聊天录音作为仓库夹具。

取得视频后，`wechat_video_observe(message_id,asset_id,mode,...)` 使用原返回的两个编号。画面模式 `mode=frames` 从 `start_frame` 起按 `step` 选择帧，返回实际索引与时间戳，从 `next_start_frame` 续读。步长为 1 时连续读取，视觉模型截断的批次会明确标记。音轨模式 `mode=audio` 从 `start_seconds` 起转写，按 `next_start_seconds` 续读。长区间按 PCM 传输上限分页，结果区分缺少音轨、该区间无音频、无可识别词和调用失败。模型只收到所选帧或 PCM 页，微信原件留在本机连接的私有目录。需要处理真实资料时，先确认该连接和所用模型服务的授权。

「回复范围」可以单独开启「自动读视频」。默认关闭，仅对开启后已获附件授权且进入有效回合的普通视频文件生效。自动读取先用已配置的音轨识别检查开头最多 45 秒，没有可用转写时再用已配置的视觉服务观察最多 6 帧。聊天模型获得实际读取范围和内容。需要继续读时仍可按需调用视频工具。这个开关与各群的视频号自动总结规则分别设置。

群聊回合中，另一位成员发的普通视频无法读取时，明确面向 Agent 且未引用该视频的独立文字请求可以继续。回合内保留这条视频未读到内容的标记。视频本身是触发请求、视频与文字由同一人发出，或有明确引用时，读取失败仍停止这次群回复。

### 普通微信语音

在连接的回复范围内为目标会话授予附件读取权限，并为该连接启用媒体理解和「本机识别」。通过当前回复规则的语音回合会先转写，再交给聊天模型判断；群里的收集窗口及上线未读汇总沿用原有触发规则。`wechat_voice_transcribe(message_id,start_seconds?,duration_seconds?,timeout_seconds?)` 可以按需读取当前或核验后的引用语音。回包区分无可辨语音、当前区间结束和处理故障，部分读取从 `next_start_seconds` 继续。转写使用原生服务的可播放 WAV，原始 SILK 仍可通过附件工具取得。普通语音当前只接入本机识别。

### 普通微信图片

为会话授予附件读取权限，在该连接的「媒体理解」配置并启用视觉服务。`wechat_image_observe(message_id,question?,analysis_tokens?)` 读取当前图片或核验后的引用图片。工具核对原图或完整标准版、实际字节和原消息。单张图片最大 8 MiB，观察被模型截断时会标记部分覆盖。

在「回复范围」开启「自动读图」后，新收到的获准图片会在符合回复条件的回合中先观察，再交给聊天模型判断。群聊沿用群触发规则，重启未读汇总也只读取开启后收到的图片。视觉服务会接收获准的图片字节，聊天模型接收观察文字和消息来源。自动读图默认关闭，撤销全部附件权限会同时关闭。修改后按页面提示恢复 Agent。

### 微信 Word 和 PDF 附件

在微信连接的「回复范围」授予该会话附件读取权限，在「项目工具」安装并启用文档工坊。引用一条 Word 或 PDF 消息提问，也可以在同一回合先发附件，再说明要查什么。大肥鱼可以按需调用 `wechat_document_inspect`，原件在本机核验，文档解析由隔离工具完成。

如果文件已经发在更早的回合，可以引用那条文件消息，也可以在会话已获准读取历史时，让大肥鱼通过 `wechat_read` 查找你先前发的文件，再用准确的消息编号读取。直接查历史只允许当前请求者此前发在当前会话的文件。要读其他成员的文件，请在已获附件授权的会话中引用对应消息。

DOCX 可按字符偏移续读。PDF 默认从第 1 页读取并优先使用文字层。带本机扫描组件时，没有文字层的页会单独渲染并识别。对于同一页既有文字又有扫描图的文件，可以显式选 `ocr_mode=always` 重读画面，也可以选 `off` 只读文字层。默认 220 DPI，可按需选 96–300 DPI；过大的页面会返回实际像素边界，降低 DPI 后可继续。

按返回的 `next_text_offset` 或 `next_page` 续读，沿用原来的 OCR 模式和 DPI。可传 `expected_sha256` 核对同一原件。结果区分文字层、OCR、低置信度、空白或无可识别文字、缺组件和处理失败。复杂表格里的文字可能漏识，旧版 `.doc` 仍待接入。读取正文不会触发发送。

构建扫描版 Windows 包时，在 PDF 导出所需参数之外提供 `-DocumentTesseractDirectory` 与 `-DocumentPDFiumWheel`。后者使用固定的 `pypdfium2-5.15.0-py3-none-win_amd64.whl`，打包校验 SHA-256 并附带 PDFium 与依赖许可证。打包后的 PDF 处理在本机完成。[固定 wheel 与许可证说明](https://pypi.org/project/pypdfium2/5.15.0/)

要导出并发回 PDF，还需为会话开启媒体回复，并安装带 PDF 运行时的文档工坊。在私聊直接请求，或在群里引用 Word 并明确向大肥鱼提出“转成 PDF 发这里”。转换使用本机排版器，版式覆盖以当前文档工坊为准。可以指定包含 `.pdf` 后缀的交付文件名。生成时记录原件、文件摘要和页数，选中成品后才进入微信发送队列。「微信已接受」以原投递回执为准。

要从图片重建文档，请在带中文 OCR 和 PDF 组件的 Windows 安装中启用文档工坊，并为该会话授予附件读取和媒体回复权限。在私聊直接请求，或在群里引用原图并对大肥鱼说「把这张图片做成可编辑 Word，再转成 PDF 发到这里」。当前支持 PNG、JPEG、标题、段落和单个表格。默认只准备 PDF，并保留可编辑的 Word 原稿。也可以明确要求交付 Word 或两种文件。检测到低置信度文字时，工具会提示核对原图。

从文档工坊 0.1.4 起，可保留原图中独立的彩色插图，周围正文和表格继续可编辑。灰度图或需要指定边界时，工具可使用 `illustration_regions`，填写原图像素坐标 `left/top/right/bottom`，右、下边界不含在裁片中。传空数组可关闭插图识别。从 0.1.5 起，支持边界清楚的矩形跨行和跨列合并。从 0.1.6 起，三行三列以上、字段对齐清楚的无边框表格也能转成可编辑单元格，PDF 中保留无边框样式。列数少、对齐不清或列间缺少留白的表格可能会按文字保留。

两栏或三栏横排纯文字中，连续留白分隔清楚时，Word 和 PDF 会先读完左栏，再读右侧各栏，输出为单栏正文。嵌套表格、倾斜版面、竖排或同页多段不同分栏仍需单独处理。

同一请求者之后可以说「把刚才的标题改大到 26 磅，再发一次」。为该会话开启历史读取、附件读取和媒体回复，且文档工坊至少为 0.1.3。工具从上一轮的 Word 原稿修改主标题，保留旧版和正文表格。多份同名文件需选定版本。当前支持 8–48 磅，精度为半磅。

本轮以合成模型替换外部传输，走真实聊天引擎、HTTP 和 SQLite，核对历史、重试、重启、遗忘和取消。页面使用真实本机服务检查准备、设置、暂停和恢复；合成气泡渲染场景单独标注。没有新增付费模型调用、真实微信消息或生产数据迁移。

```powershell
python -B scripts/test_local_home_ui.py `
  --binary temp/local-home/agent-home-station.exe `
  --output temp/local-home/browser
```

回放需要 Python Playwright 和 Edge，测试目录及截图不进入 Git。5.B.3 的真实 Windows LPAC 验证使用隔离合成文件：创建并执行 Python 工具、生成作品、拒绝读取宿主私有测试文件、相同编号不重复启动、暂停后取消进程，重启仍可查原记录。没有调用付费模型或微信。应用页的安装与账号状态使用真实本机 HTTP 和合成账号回放验收。后续继续接入微信登录与启停；自动长期记忆整理和进程完成续办也有各自的后续阶段。


## 创作草稿与发布任务

在软件聊天里，可以让大肥鱼制作文件、登记成品，并把成品保留为创作草稿。例如：“把刚才的 PDF 和可编辑 Word 记成一份本地草稿，先不发布。”草稿保留可编辑源与成品的关联，重启后可以按任务编号查询或取消。没有接入应用时也可以保存草稿。

发布目标只有在适配器支持且主人授权有效时才会出现在 `app_publish(action=targets)` 的结果里。抖音需要在扫码时单独选择发布权限。授权过期后，创作发布区提示续期或重新扫码，已保存的任务和已核对的回执仍可查看。Windows 隔离包已核对过期后的目标过滤，真实平台投稿另行验收。

OBS 本地预览安装时需授权访问本机 WebSocket，配置并启用后才可主动取帧。直播需要另外核对本地编码器及平台授权，两项均可用时才有直播目标。

已绑定目标的草稿通过原任务编号提交，传输超时先查询，不能重新建同一作品的发布任务来重发。取消可以阻止尚未提交的操作。平台已经接受的作品会保留实际结果，取消不等于撤回。
