# 音频工坊

音频工坊是 Windows 本机项目工具的一个受控 MCP 包。它将当前任务已登记的整数 PCM WAV 裁剪为所需片段，或将多声道混合为单声道，输出新的 16 位 PCM WAV 成品。

它不负责录音识别、TTS、声线训练、RVC 换声、音乐生成或微信发送。这些能力会在后续以各自的项目工具接入。宿主负责素材授权、成品登记、投递及回执。

## 输入与范围

- 只接受宿主在 `_meta["station/audio"]` 中绑定的一份 PCM WAV。宿主调用时会绑定 `artifact.artifact_id` 和 `artifact.sha256`，不能提供本地路径。
- 支持 8、16、24、32 位整数 PCM WAV，输出恒为原采样率的 16 位 PCM WAV。压缩 WAV、浮点 WAV、空音轨、格式异常或超限文件明确失败。
- `start_seconds` 和 `duration_seconds` 均按采样帧换算。读取到文件末尾时结果会标记 `coverage.eof=true` 并报告实际时长，不会假称取得了请求范围。
- `channels=mono` 以同一帧全部声道的整数平均值生成单声道。默认 `preserve` 保留原声道数。
- 输入和输出均限制为 512 MiB。8 位输入转为 16 位输出后如会超出输出上限，工具返回 `audio_output_too_large`，可缩短时长后续办。
- 读取前会核对 RIFF 数据块声明与实际字节数。文件被截断时返回 `audio_wav_truncated`，不会把损坏文件误报为正常读到结尾。

## 离线验证

```powershell
python -B plugins/audio-workshop/tests/test_audio_workshop.py
```

测试只使用临时目录中的合成 WAV，不访问网络、微信、模型或真实录音。
