# 运行时协议

`main.py` 是 MCP stdio 入口。宿主在 `_meta["station/audio"]` 中传入唯一的 `input`、`input_root`、`output_dir` 和 `output_root`。这些都是任务进程可见的受控路径，不能由聊天内容、模型参数或外部资料选择。

入口仅调用 Python 标准库的 `wave` 模块和同目录 `audio_logic.py`。它不启动外部程序、桌面窗口或网络连接。
