# 文档工坊

文档工坊是可选的本地 MCP 工具。它读取当前任务已授权的图片、DOCX 或 PDF，返回文本/版式观察；可将已经核验的版式制作成可编辑 DOCX，导出 PDF 并检查成品。

## 使用边界

1. 聊天中的附件先由主体登记为素材；主体将授权副本和受控输出目录绑定到本次调用。
2. `document.inspect` 只报告已经读取到的内容。DOCX 的 `include_layout=true` 返回连续文档流中实际可编辑的标题、层级标题、段落和表格；它尚未渲染时没有稳定页码，传入非空 `pages` 会明确返回 `document_docx_page_selection_unsupported`。正文用 `text_offset`/`text_limit`（默认 4,000 字符）续读，结构用 `start_block`/`block_limit`（默认 20 块）续读；结果明确给出 `text_eof`、`next_text_offset`、`layout.coverage.eof` 和 `next_start_block`，不会静默截断。扫描件 OCR 不可用、关键金额/日期/编号不清晰时会返回 `needs_clarification`，不会猜测。
3. `document.reconstruct` 使用 Word 段落和表格，而不是把整页截图粘进 DOCX；原图或裁切图可作为插图。
4. `document.export_pdf` 直接在受管 Python 中导出 PDF，不启动 LibreOffice、Office 或桌面程序。首版覆盖本工具产出的段落、标题、简单表格与嵌入图片；在受限 Windows 环境中，图片走纯 Python 路径：仅支持非隔行、8 位 RGB/RGBA PNG，以及 8 位灰度/RGB baseline 或 progressive JPEG。调色板 PNG、灰度 PNG、CMYK JPEG、损坏或超限图片会明确返回 `native_pdf_embedded_image_unsupported`，不会静默省略图片。复杂的既有 DOCX 会明确返回 `native_pdf_layout_unsupported` 等边界，而不会悄悄改变版式。
5. `document.verify` 做结构、文本和可选渲染检查；它发现的是技术质量，不代替人工审美或内容审批。

输入与输出路径只在宿主绑定的 MCP 上下文中传递。工具会拒绝任何来自工具参数的路径、命令、URL 或凭据。

## 离线验证

```powershell
python plugins/document-workshop/tests/test_document_workshop.py
```

测试使用安全合成版式和临时任务目录，不访问微信、网络、模型或真实文件。中文字体优先使用已审计的项目内字体；Windows 系统字体只能作为明确标记的本机后端，不能当作可移植运行时证据。
