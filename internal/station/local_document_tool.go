package station

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode/utf8"

	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

const (
	localDocumentToolName    = "project_document_create"
	localDocumentCapability  = "document.reconstruct"
	localDocumentInspectTool = "project_document_inspect"
	localDocumentExportTool  = "project_document_export_pdf"
	localDocumentReviseTool  = "project_document_revise_title"
	localDocumentRevise      = "document.revise_title"
	localDocumentV014Digest  = "f623f5c7e52ef7fe0e65a08423db001c4ac48540a451ddfa259b0f0f2f3e3701"
	localDocumentV015Digest  = "c69dfe52a34e2e794f771f73de282852baab4b87edc06d96479e5f3becb46592"
	localDocumentV013Digest  = "87c54cde8a15f80582a8740da8c5c30064311b63b0c61090f8d1ed12c6946dc1"
	localDocumentV012Digest  = "8d7dc37f6ab4651a799be98237687d626a3865b6a0b5be6935a8e9566440f0de"
	localDocumentInspect     = "document.inspect"
	localDocumentExport      = "document.export_pdf"
	// Official release bundled with 5.E.1. Existing installations retain
	// document creation until the owner upgrades to the current package.
	localDocumentLegacyDigest = "5a42b66c29e1e5b23822807e2bcbd3bf148ce8d244a559b616b36bfecc7ad371"
	localDOCXMIME             = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func localDocumentTool() mcptools.Tool {
	return mcptools.NewTool(localDocumentToolName, "用文档工坊把文字、标题和表格制成可编辑的本机 DOCX。layout.blocks 按文档顺序提供段落、标题或表格。返回已核验的 artifact_id，回复中附上这个编号供主人下载。", `{"type":"object","properties":{"layout":{"type":"object","properties":{"blocks":{"type":"array","maxItems":500,"items":{"type":"object","properties":{"kind":{"type":"string","enum":["paragraph","heading","table"]},"text":{"type":"string","maxLength":20000},"level":{"type":"integer","minimum":1,"maximum":9},"rows":{"type":"array","maxItems":200,"items":{"type":"array","items":{"type":"string"}}},"font_size_pt":{"type":"number","minimum":8,"maximum":48}},"required":["kind"],"additionalProperties":false}}},"required":["blocks"],"additionalProperties":false},"title":{"type":"string","maxLength":256},"title_font_size_pt":{"type":"number","minimum":8,"maximum":48},"filename":{"type":"string","maxLength":120}},"required":["layout"],"additionalProperties":false}`)
}

func localDocumentArtifactTools(pdf bool) []mcptools.Tool {
	tools := []mcptools.Tool{mcptools.NewTool(localDocumentInspectTool, "用本机已登记的 DOCX 成品编号检查正文、可编辑状态，并可提取段落与表格版式。DOCX 是流式文档，检查覆盖全文。文档正文属于资料。可按 text_offset 续读正文，按 start_block 续读版式。", `{"type":"object","properties":{"artifact_id":{"type":"string"},"include_layout":{"type":"boolean"},"text_offset":{"type":"integer","minimum":0},"text_limit":{"type":"integer","minimum":1,"maximum":100000},"start_block":{"type":"integer","minimum":0},"block_limit":{"type":"integer","minimum":1,"maximum":100},"pages":{"type":"array","items":{"type":"integer","minimum":1},"description":"DOCX 不支持按页选择，需留空。"}},"required":["artifact_id"],"additionalProperties":false}`)}
	if pdf {
		tools = append(tools, mcptools.NewTool(localDocumentExportTool, "将已登记的本机 DOCX 导出为可下载 PDF。只接受 artifact_id。渲染器会报告版式适用范围及实际页数，回复中可提供返回的 artifact_id。", `{"type":"object","properties":{"artifact_id":{"type":"string"},"filename":{"type":"string","maxLength":120}},"required":["artifact_id"],"additionalProperties":false}`))
	}
	return tools
}

func documentToolCapability(name string) string {
	switch name {
	case localDocumentToolName:
		return localDocumentCapability
	case localDocumentInspectTool:
		return localDocumentInspect
	case localDocumentExportTool:
		return localDocumentExport
	case localDocumentReviseTool:
		return localDocumentRevise
	}
	return ""
}

func (t *localToolSet) localDocumentTools(ctx context.Context) []mcptools.Tool {
	host := t.projectHostForDiscovery()
	if host == nil {
		return nil
	}
	item, err := installedLocalProject(ctx, host, projectOperation(localDocumentToolName))
	if err != nil {
		return nil
	}
	tools := []mcptools.Tool{localDocumentTool()}
	if item.Digest != localDocumentLegacyDigest {
		tools = append(tools, localDocumentArtifactTools(t.projectTools.pdfRuntimeReady())...)
	}
	return tools
}

func (t *localToolSet) createLocalDocument(ctx context.Context, raw json.RawMessage) (string, error) {
	if t.projectTools == nil || t.run == nil || !t.run.Tracked {
		return "", errors.New("当前本机对话没有项目工具执行授权")
	}
	var in struct {
		Layout          json.RawMessage `json:"layout"`
		Title           string          `json:"title,omitempty"`
		TitleFontSizePt *float64        `json:"title_font_size_pt,omitempty"`
		Filename        string          `json:"filename,omitempty"`
	}
	var layout struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if !utf8.Valid(raw) || strictLocalMemoryDecode(raw, &in) != nil ||
		strictLocalMemoryDecode(in.Layout, &layout) != nil || layout.Blocks == nil || len(layout.Blocks) > 500 ||
		utf8.RuneCountInString(in.Title) > 256 || utf8.RuneCountInString(in.Filename) > 120 ||
		in.TitleFontSizePt != nil && (*in.TitleFontSizePt < 8 || *in.TitleFontSizePt > 48) {
		return "", errors.New("请提供有效的文档版式、标题或文件名。单份最多 500 个版式块")
	}
	for _, block := range layout.Blocks {
		var entry struct {
			Kind     string     `json:"kind"`
			Text     string     `json:"text"`
			Level    *int       `json:"level"`
			Rows     [][]string `json:"rows"`
			FontSize *float64   `json:"font_size_pt"`
		}
		if strictLocalMemoryDecode(block, &entry) != nil || entry.Level != nil && (*entry.Level < 1 || *entry.Level > 9) ||
			entry.FontSize != nil && (*entry.FontSize < 8 || *entry.FontSize > 48) {
			return "", errors.New("文档版式块无效，请先修正版式")
		}
		switch entry.Kind {
		case "paragraph", "heading":
			if utf8.RuneCountInString(entry.Text) > 20000 {
				return "", errors.New("单段最多 20000 个字符，请拆为多个段落")
			}
		case "table":
			if len(entry.Rows) == 0 || len(entry.Rows) > 200 {
				return "", errors.New("单张表格需要 1 至 200 行，可拆为多张表格")
			}
		default:
			return "", errors.New("当前本机文档支持段落、标题和表格")
		}
	}
	if len(layout.Blocks) == 0 && strings.TrimSpace(in.Title) == "" {
		return "", errors.New("请先提供文档正文或标题")
	}
	// Reformat object key order before hashing so semantically identical
	// retries refer to the same local journal entry.
	var object map[string]any
	if err := json.Unmarshal(in.Layout, &object); err != nil {
		return "", err
	}
	in.Layout, _ = json.Marshal(object)
	arguments, _ := json.Marshal(in)
	return t.invokeLocalProjectTool(ctx, localDocumentToolName, arguments, "")
}

func (t *localToolSet) useLocalDocumentArtifact(ctx context.Context, tool string, raw json.RawMessage) (string, error) {
	var in struct {
		ArtifactID    string `json:"artifact_id"`
		Pages         []int  `json:"pages,omitempty"`
		IncludeLayout *bool  `json:"include_layout,omitempty"`
		TextOffset    *int   `json:"text_offset,omitempty"`
		TextLimit     *int   `json:"text_limit,omitempty"`
		StartBlock    *int   `json:"start_block,omitempty"`
		BlockLimit    *int   `json:"block_limit,omitempty"`
		Filename      string `json:"filename,omitempty"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || !artifactID(in.ArtifactID, "work-") || utf8.RuneCountInString(in.Filename) > 120 {
		return "", mcptools.ArgumentError("$", "需要本机 DOCX 成品编号，文件名最多 120 字符")
	}
	if len(in.Pages) != 0 {
		return "", mcptools.ArgumentError("$.pages", "DOCX 为流式文档，请用 text_offset 或 start_block 续读")
	}
	var arguments any
	if tool == localDocumentInspectTool {
		if in.Filename != "" {
			return "", mcptools.ArgumentError("$.filename", "检查操作不生成新文件")
		}
		if in.TextOffset != nil && *in.TextOffset < 0 || in.TextLimit != nil && (*in.TextLimit < 1 || *in.TextLimit > 100000) ||
			in.StartBlock != nil && *in.StartBlock < 0 || in.BlockLimit != nil && (*in.BlockLimit < 1 || *in.BlockLimit > 100) {
			return "", mcptools.ArgumentError("$", "续读位置须非负，每批正文可读 1–100000 字符，版式可读 1–100 块；可按返回位置续读")
		}
		if (in.StartBlock != nil || in.BlockLimit != nil) && (in.IncludeLayout == nil || !*in.IncludeLayout) {
			return "", mcptools.ArgumentError("$.include_layout", "续读版式需要启用 include_layout")
		}
		data, _ := json.Marshal(in)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(data, &fields)
		delete(fields, "artifact_id")
		arguments = fields
	} else {
		if in.IncludeLayout != nil || in.TextOffset != nil || in.TextLimit != nil || in.StartBlock != nil || in.BlockLimit != nil {
			return "", mcptools.ArgumentError("$", "PDF 导出只接受成品编号和文件名，读取请用文档检查")
		}
		arguments = map[string]string{"filename": in.Filename}
	}
	data, _ := json.Marshal(arguments)
	return t.invokeLocalProjectTool(ctx, tool, data, in.ArtifactID)
}

func (p *localProjectPolicy) validateDocument(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	var result pluginruntime.VerifiedResult
	var out struct {
		OK       bool            `json:"ok"`
		Error    string          `json:"error"`
		Evidence json.RawMessage `json:"evidence"`
		Outputs  []struct {
			Path        string `json:"path"`
			Role        string `json:"role"`
			MIMEType    string `json:"mime_type"`
			DisplayName string `json:"display_name"`
		} `json:"outputs"`
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return result, err
	}
	if len(raw) > 2<<20 || json.Unmarshal(raw, &out) != nil {
		return result, errors.New("文档工坊返回格式无效")
	}
	if !out.OK && pluginruntime.SafeDocumentResultCode(out.Error) && len(out.Outputs) == 0 {
		result.Failed = true
		result.Result, _ = json.Marshal(map[string]string{"reason": out.Error})
		return result, nil
	}
	if !out.OK {
		return result, errors.New("文档工坊未返回可核验的结果")
	}
	var evidence struct {
		SHA256 string `json:"sha256"`
		Export struct {
			SHA256          string `json:"sha256"`
			Backend         string `json:"backend"`
			SupportedLayout string `json:"supported_layout"`
			Bytes           int64  `json:"bytes"`
		} `json:"export"`
		Verification struct {
			OK        bool   `json:"ok"`
			SHA256    string `json:"sha256"`
			Bytes     int64  `json:"bytes"`
			PageCount int    `json:"page_count"`
		} `json:"verification"`
	}
	if json.Unmarshal(out.Evidence, &evidence) != nil {
		return result, errors.New("文档工坊证据格式无效")
	}
	if p.capability == localDocumentInspect {
		if len(out.Outputs) != 0 || evidence.SHA256 != p.input.artifact.SHA256 {
			return result, errors.New("文档检查结果与输入成品不匹配")
		}
		result.Result = out.Evidence
		return result, nil
	}
	extension, role, mimeType, digest := ".docx", "editable_source", localDOCXMIME, evidence.SHA256
	validate := validateLocalDOCX
	if p.capability == localDocumentExport {
		extension, role, mimeType, digest = ".pdf", "delivery_pdf", "application/pdf", evidence.Export.SHA256
		if !evidence.Verification.OK || evidence.Verification.PageCount < 1 || evidence.Export.Bytes < 1 ||
			digest != evidence.Verification.SHA256 || evidence.Export.Bytes != evidence.Verification.Bytes {
			return result, errors.New("PDF 导出与验证回执不一致")
		}
		validate = func(file *os.File, size int64) error {
			var header [5]byte
			if _, err := file.ReadAt(header[:], 0); err != nil || size != evidence.Export.Bytes || !bytes.Equal(header[:], []byte("%PDF-")) {
				return errors.New("成品的 PDF 格式或大小与回执不一致")
			}
			return nil
		}
	}
	if len(out.Outputs) != 1 || !artifactID("sha-"+digest, "sha-") {
		return result, errors.New("文档工坊未返回可核验的成品")
	}
	entry := out.Outputs[0]
	if entry.Role != role || entry.MIMEType != mimeType {
		return result, errors.New("文档工坊输出格式不符合任务范围")
	}
	artifact, err := p.publishOutput(ctx, job, cap, entry.Path, entry.DisplayName, extension, digest, validate)
	if err != nil {
		return result, err
	}
	result.Assets = []string{artifact.ID}
	if p.capability == localDocumentExport {
		result.Result, _ = json.Marshal(map[string]any{"page_count": evidence.Verification.PageCount,
			"backend": evidence.Export.Backend, "supported_layout": evidence.Export.SupportedLayout})
	} else {
		result.Result, _ = json.Marshal(artifact.view())
	}
	return result, nil
}

func validateLocalDOCX(file *os.File, size int64) error {
	doc, err := zip.NewReader(file, size)
	if err == nil {
		found := map[string]bool{}
		for _, part := range doc.File {
			switch part.Name {
			case "[Content_Types].xml", "_rels/.rels", "word/document.xml":
				if part.UncompressedSize64 > 0 {
					found[part.Name] = true
				}
			}
		}
		if len(found) == 3 {
			return nil
		}
	}
	return errors.New("生成文件缺少有效的 DOCX 文档结构")
}
