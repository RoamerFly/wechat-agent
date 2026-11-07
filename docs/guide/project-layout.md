# 目录与启动入口

根目录日常只需关注 `启动大肥鱼.bat` 和README。源码保持Go惯例，运行脚本保留稳定路径，避免现有服务、计划任务或Agent指南失效。

```text
wechat-bot/
├─ 启动大肥鱼.bat        双击启动已有后台并打开网页
├─ README.md            快速开始与目录导航
├─ cmd/                 Go程序入口
├─ internal/            后端、内嵌网页、测试
├─ scripts/             启动/桥接/同步/检查脚本
├─ components/          关联项目源码和来源清单
├─ docs/
│  ├─ guide/            使用、功能、工具、目录
│  ├─ agent/            Agent、工具、权限与生命周期
│  ├─ delivery/         发送、回执、补发
│  ├─ media/            语音、音乐、视频与分享
│  ├─ ui/               网页和目录交互
│  ├─ mobile/           旧设备功能与历史兼容资料
│  └─ design/           自主冲浪与发言设计参考
├─ deploy/docker/       历史容器配置草稿
├─ examples/、configs/   示例与实验配置
├─ design/              用户选定视觉资产与设计清单
├─ research/            开发研究、补丁、复核脚本
├─ secrets/             私有凭据，勿删除或上传
├─ temp/                日志和任务媒体，勿整体清空
└─ dist/                本机构建产物
```

已归并52个文档/部署文件；删除6份过期或重复根目录文档，以及固定打包旧0.3.0镜像的导出脚本。使用说明和功能清单重写为当前入口，减少历史版本叙述和矛盾指引。旧文件可从Git历史恢复。

清理仅删除已确认的Python字节码缓存和空目录，没有删除歌曲、参考音频、下载文件、真实聊天、模型或研究证据。WSL私有数据目录保持原位；根目录现有私有Key文件也保留原路径。

启动器日志位于 `temp/launcher/`。正常运行不留下常驻命令行窗口；失败提示会指向日志。BAT按自身目录找脚本，支持从其他工作目录启动。直接调试入口为：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/Launch-Local.ps1 -Action Start -NoBrowser -NoDialog
```

`-CheckOnly`只检查入口和依赖，不启动服务。默认Start只启动微信和管理端；`Music`按需启动YuE，避免每次打开管理页都加载音乐环境。两种入口均不重新启用登录Windows自启。

本次验证包括中文/空格路径、非项目工作目录、Windows PowerShell 5实际启动、BAT转发、文档链接、容器上下文静态检查及完整源码验证。没有以测试为由发送微信消息，也没有执行容器构建或镜像导出。
