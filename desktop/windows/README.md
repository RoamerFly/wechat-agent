# Windows 桌面程序

应用化重构的零应用预览使用 `./scripts/Build-Desktop.ps1 -LocalStation -OutputDirectory temp/local-station-preview`，打开输出的 `AgentHomeStation.exe`。它只启动本机聊天和家目录服务，使用独立数据、单例与控制通道，不要求微信原生包，也不会停止旧微信实例。[独立入口说明](../../docs/guide/local-home-preview.md)包含目录、配置和隔离 `-Test` 验证方式。下面描述仍可兼容使用的原微信桌面入口；尚未将真实登录和历史自动迁入新入口。

双击 `WechatAgent.exe` 自动启动本机服务，窗口直接显示现有管理页，没有额外的启动控制台或外部浏览器窗口。右上角 **× 关闭窗口** 默认弹窗选择“取消／最小化到托盘运行／退出程序”，未勾选“记住此选项”时下次仍询问。选择退出才等待在途任务、停止全部本实例服务并退出；选择托盘则保持服务运行。可从托盘菜单“关闭窗口时”随时改回“每次询问”。普通 **最小化** 保留任务栏按钮；仅在关闭选择托盘或使用托盘菜单时隐藏窗口，双击托盘图标返回管理页。进入托盘不弹气泡提醒。托盘菜单另提供启动、停止服务、关于及明确的“退出并停止全部服务”。启动服务不会更改管理页中的 Agent 自动回复开关。

首次打开默认最大化；收起到托盘后恢复上次的最大化/普通窗口状态。Windows 原生 EXE 直接在 Windows 执行，但当前上游底层使用 Linux 微信协议及设备标识，因此手机确认页会显示 Linux 电脑；这一标识不代表程序通过 WSL 运行，也不表示已切换为官方 Windows 客户端协议。

支持两个后端：独立便携包默认使用 **Windows 原生**；已有 `secrets/local-deployment.json` 的 `backend=wsl` 仍按原方式管理 WSL 服务，不自动迁移或覆盖资料。Docker 部署继续使用原入口，桌面版不会擅自控制 Docker。

## Windows 原生便携包

开发机执行：

```powershell
./scripts/Build-Desktop.ps1 -OutputDirectory dist/windows-native -Test -Portable
```

新 Windows 桌面构建从分发目录移除了旧手机控制工具和桥接脚本。若目标目录已有这些组件，构建会保留原目录并要求另选输出位置。旧设备任务与素材属于原部署的数据目录。原生 Windows 旧管理页可从工具列表的设备历史档案查看已保留的任务、截图和录屏；历史会话里的设备记录也可跳到该档案。

首次源码构建需要 Go、Git、Python 3.13、FFmpeg/FFprobe 分发目录、Rust 1.93.1、MSVC C++/Windows SDK 和完整的原生 Windows Perl（例如 Strawberry Perl，可通过构建环境变量 `PERL` 指定）。这些是构建依赖，使用者无需安装。`Build-WindowsNative.ps1` 固定使用 `components/native-client/source-manifest.json` 的上游提交与累计补丁，核验补丁哈希；构建 `wx.exe` 和无 CGO 的 `wechat-admin.exe`，同时生成 SHA-256 清单和上游许可证。Go 管理端从本仓库当前提交的隔离快照构建，排除已有未提交工作；通常由清单记录该提交；本地开发可显式传入 `-GoSourcePatch <任务补丁>`，在隔离快照验证并应用，仅允许管理源码路径，并记录补丁 SHA-256。原生构建使用较短的 `temp/wn-target`，避免 OpenSSL/MSVC 嵌套长路径失败。原生程序位于便携包 `runtimes/windows-x64/`，不打包聊天和模型。`Prepare-WindowsResources.ps1` 将校验过的 Python CLI 和 FFmpeg/FFprobe 放在独立的 `runtimes/windows-python-cli`、`runtimes/windows-media`，保留版本、文件哈希与许可；可用 `-MediaDirectory` 指定实际媒体工具分发目录，不能复制依赖系统安装的 shim。程序包须整体复制。

双击便携包里的 EXE 即可运行，不要求 WSL、Docker、Python、Node 或官方微信桌面版。首次在管理页扫码并配置模型及会话；Agent 默认未启用。系统需有 WebView2 Runtime。程序包包含从 MSVC 构建工具可再发行目录取得的 app-local DLL，并自带媒体工具，优先从项目目录加载。首次启动使用空的独立资料，不能读取官方 Windows 微信旧聊天。Linux 音乐环境与既有私有模型不自动登记为 Windows 可运行资源；本地 ASR 可使用项目内的 Windows 专用运行环境重新登记。

无配置时数据根为程序目录的 `data/`，端口为管理页 `8081`、微信后台 `6174`，账号 ID 为 `desktop`。可在 `secrets/local-deployment.json` 显式配置数据根和回环端口：

```json
{"backend":"windows-native","data_root":"data","account":"desktop","admin_url":"http://127.0.0.1:8081","native_url":"http://127.0.0.1:6174"}
```

相对数据根按程序工作区解析，不依赖当前工作目录，需放在支持 Windows ACL 的可写磁盘（如 NTFS）。数据、密钥和日志由当前用户与 SYSTEM 独占；链接/重解析路径、校验失败的程序、非回环地址及占用端口拒绝启动，不接管其他实例。`data/logs/` 记录启动失败。页面会话在 `secrets/`。升级保留 `data/`、`secrets/`、模型和生成物。

Windows 后台使用前台 `wx serve` 子进程，由桌面进程统一管理；使用 Windows Job Object 管理本实例及其子进程。正常退出先鉴权排空全部账号并以只读 SQLite 查询核验原生附件队列，随后通过继承的私有 stdin 管道让 Go 服务优雅退出，再使用绑定实例身份的原生停止协议停止微信；不登出账号，不增加公开停止接口，不依赖 Python。桌面进程意外终止时 Job Object 结束其所属进程树，持久任务由后续启动恢复。新安装不导入或启动已有 WSL 实例。

运行验证命令（仅用于独立空白数据根，禁止指向唯一生产副本）：

```text
DesktopTests.exe --cycle <隔离便携包目录>
```

它真实启动/停止两轮 Windows 微信和 Go 服务，但不扫码、不触发模型、不发送消息。真实登录、原有资料迁移及手机媒体播放另行验收，不能以空账号或模拟测试替代。

## 构建和使用

在 Windows 运行一次构建（使用系统 .NET Framework C# 编译器）：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/Build-Desktop.ps1 -Test
```

产物在 `dist/windows-desktop/WechatAgent.exe`。它是 Windows x64 GUI 可执行文件，内嵌 SDK 程序集及加载器，无需从 PowerShell、BAT 或 Python 启动。运行时直接调用隐藏的 `wsl.exe` 和现有 systemd 单元；Python 3 仅用于 WSL 内的只读在途任务检查，已有本机部署已具备此依赖。

放在项目子目录中可自动找到项目根，也支持：

```text
WechatAgent.exe --workspace "E:\项目目录"
```

可以为此命令创建桌面快捷方式。关闭、启动与重复双击都只作用于这一个项目实例；同一 Windows 会话仅允许一个桌面程序；重复双击、不同路径的程序副本和其他 `--workspace` 参数都只激活已有窗口，不再创建管理页或启动第二组服务。已有实例正在退出时不会重新唤起窗口。未知后端、异常服务名或账号不匹配会拒绝操作。仅构建桌面壳时，现有 WSL 使用方式如下。

## 退出和数据边界

停止时通过现有管理员鉴权、CSRF 和全部账号排空协议暂停接受新处理，等待在途 Agent、维护、手机及后台程序；只读检查各账号原生附件队列和音乐任务。等待可取消，并解除排空。检查失败或未知时保持运行，不强制杀掉任务。

排空后依次停止 Web、受管音乐、原生微信服务，并检查其确实变为 inactive。停止 systemd 单元会断开当前在线连接，但不退出微信账号或删除登录资料。重新启动使用原连接资料，必要时要求原生服务连接已保存账号。不会调用 WSL shutdown/terminate，也不结束其他项目或整个发行版。操作已进入 systemd 停止阶段时不再接受“取消”，以实际完成结果为准。

关闭窗口等待停止成功后才退出；停止失败保留窗口及具体错误，等待期间可“取消退出”。登录、配置和模型保留。程序不会创建新微信消息、重放任务或改变 Agent 自动回复启停设置。停止后也结束仅属于本项目的旧启动器和桌面程序 WSL 保活进程，匹配完整实例标识及 argv，不结束其他 WSL 会话；旧手动计划任务定义可以保留，但已结束的保活进程不会被手动任务自动拉起。桌面程序不新增计划任务或开机启动项。

## 内嵌页面和依赖

使用 WinForms 与系统 WebView2 Runtime；本机已有 Runtime 时无需下载安装浏览器。缺少 Runtime 时会显示错误，服务仍可通过托盘菜单安全停止。SDK `Microsoft.Web.WebView2` 固定 `1.0.3537.50`，从[官方 NuGet 包](https://www.nuget.org/packages/Microsoft.Web.WebView2/1.0.3537.50)下载，并核验包 SHA-256：

```text
5ea526bbd728adda0da4d31219267e96460494a427e4894c4e09d9f320f4b9aa
```

SDK 许可为包内 Microsoft WebView2 SDK License，构建时同时输出 `WebView2-LICENSE.txt`，分发 EXE 时保留此许可。加载器从 EXE 提取到项目 `temp/desktop/runtime/<hash>` 并核验内容；页面登录状态保存在被 Git 忽略的 `secrets/desktop-webview`。不向页面脚本注入密码或提供宿主执行对象；只允许当前本机管理源在内嵌视图导航，用户点击的外站链接交给外部浏览器。

构建依赖缓存、Runtime、页面状态及生成物均不入 Git。程序和测试源码位于本目录，CI 的 Windows job 编译 GUI EXE 并执行控制器回归；Linux Source checks 继续验证服务端。没有生成 Docker 镜像或发布 Release。

迁移旧 WSL 管理资料见[Windows 数据迁移](../../docs/guide/windows-migration.md)。需要分离程序与数据时，部署配置可设置 `runtime_directory` 指向包含原生程序及校验清单的目录；`data_root` 独立保存用户资料。便携包已提供 Pythonw 与 LPAC 代理脚本，桌面程序仅为缺省配置提供传输路径，不启用或扩大 Agent 权限；已明确登记的执行器不会被覆盖。

## 项目内语音环境

`prepare_windows_python.py` 固定下载 Python 3.13.13 的官方嵌入包并核验 SHA-256；缓存保留原包。LPAC 无界面版本移除 Python EXE、DLL 和扩展模块（包括 `_ctypes.pyd`）的 PE 启动清单，以免 Windows SxS 激活上下文在隔离环境中于 Python 启动前返回 `0xC0000022`。这是记录转换及文件哈希的派生 CLI 环境，上游发布者签名不再适用于改动文件，不作为通用 Python/Tk 桌面环境。隔离能力、网络权限与系统 ACL 均不因此扩大。

使用已下载的、哈希固定的模型归档安装 Windows ASR：

```powershell
python -X utf8 scripts/install_local_asr.py --workspace <数据根>/managed-workspaces --archive <模型归档.tar.bz2>
```

安装器在项目内准备 Python、固定版本的 NumPy/Sherpa SDK、模型、许可证、脚本与登记清单；SDK 中同样触发 SxS 的清单转换也记录原始/派生哈希。输入只存于当前账号私有 scratch，实际推理仍在 LPAC 中执行。覆盖旧的 Linux ASR 路径登记需明确添加 `--replace-registration`，旧登记会先备份。大型模型、SDK 和下载缓存不放入 Git，不随普通程序升级覆盖。Windows 音乐/CUDA 环境独立于普通 Agent，验收记录见 Windows 原生验证文档。后续工具可分别引用这些版本化资源目录，无需将运行环境塞进 EXE 或混入用户数据库。


## Windows 音乐资源与工具边界

Windows 音乐控制脚本位于程序的 `runtimes/windows-music`；公共模型/源码与 Python 包放在数据根 `managed-workspaces/public_workspace`，个人声线和生成物留在账号私有目录。普通程序升级不替换这些大资源。基本程序便携包不包含个人声线或大型音乐模型；已有本机资源经过校验和显式登记后使用，无需 WSL。

已准备的 Windows CUDA 包放入 `public_workspace/runtimes/windows-music-packages`，补充依赖放入 `windows-music-extras`。GPT-SoVITS 和 RVC 训练依赖分别使用 `windows-gpt-packages`、`windows-rvc-training-packages`，避免更改其他模型的 Transformers/Matplotlib。固定的 GPT 推理及训练依赖清单位于 `scripts/gpt_sovits_windows_requirements.txt`、`scripts/rvc_training_requirements.txt`；安装使用项目内 `--target`，不替换系统 Python。资源登记器记录已安装版本、解释器派生文件哈希，保持模型及索引哈希和声线身份。

经拥有者明确授权后，使用 `scripts/register_windows_music.py --help` 指定当前工作区、留存公共资源、管理数据和程序运行库，并加 `--authorize-registry-and-private-network`。该登记不启动模型、重放任务或发送微信。首次迁移修复明确资源树内部的 ACL 继承；已验证的迁移可显式使用 `--inheritance-already-verified`。原配置更改前在相应目录的 `.backups` 子目录留备份，不将备份作为活跃项目登记发现。

在管理页启动已登记音乐服务，或由授权 Agent 调用 `music_service_start`，再检查实际就绪状态。仅音乐/GPU 后端具有注册表读取和私有网络能力，该能力可能访问局域网；公共模型只读、当前账号私有目录可写，控制凭据不在挂载内，普通 Agent 不获得相应能力。命名声线有独立固定参数执行入口，不能自动退回默认大肥鱼声线。后续工具可沿用资源清单、账号范围及受管生命周期，无需把环境放进 EXE 或混入记忆数据库。


## 窗口生命周期与图标

普通最小化交给 Windows 任务栏管理，托盘隐藏使用原窗口的 Hide/Show，不切换 ShowInTaskbar，避免重建宿主 HWND。退出暂停窗口激活，撤销旧 WebView 初始化与导航回调，WebView2 加载器每进程只配置一次，服务排空完成后将最终关闭投递到下一轮消息循环，避免 FormClosing 内重入关闭和已销毁窗口再次显示。

`DesktopInstance` 在读取部署配置和创建任何 Form 前取得单例互斥锁。次实例只发送激活事件后退出；互斥锁由操作系统释放，异常退出后可以重新启动。服务仍按部署身份核验归属，不收养其他服务。

项目图标为 gpt-image 生成的“聊天＋工具”抽象图形。原始透明 PNG 和多尺寸 ICO 位于 `desktop/windows/assets`；构建通过 `/win32icon` 嵌入 EXE，主窗口与托盘读取同一应用图标。`scripts/build_windows_icon.py` 从保留的 PNG 生成 16、20、24、32、40、48、64、96、128、256 像素 ICO，校验透明度及全部尺寸。

`Build-Desktop.ps1 -Test` 除控制器测试外，通过 `WindowTestRunner` 在独立、不会切换到前台的 Windows Desktop 中运行真实 WinForms/WebView 生命周期测试，使用合成服务覆盖普通最小化、显式托盘、窗口句柄稳定、页面初始化期间退出、重复激活与异步关闭、12 个并发次实例及异常退出恢复，不发送微信消息。


启动与退出使用双缓冲的局部进度动效，不改变 Form.Opacity 或重建 HWND。关闭完成后短暂显示真实停止结果，再退出；取消排空保留原页面。系统关闭客户端动画时停用非必要动画。正式程序设置固定 AppUserModelID，窗口生命周期仅记录到本机 `secrets/desktop-windows.jsonl`，便于区分进程、副本与窗口，不记录聊天正文。
