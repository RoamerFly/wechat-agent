package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
)

// stationDocumentExportAvailable deliberately requires the document runtime,
// unlike DOCX inspection.  Inspecting DOCX is implemented in-process by the
// workshop, while PDF production uses the separately installed converter.
func (h *localHomeService) stationDocumentExportAvailable(ctx context.Context) bool {
	return h.stationDocumentToolAvailable(ctx, projectOperation(localDocumentExportTool), true)
}

func (h *localHomeService) exportStationDocumentPDF(ctx context.Context, in stationDocumentInput, filename string,
	persist func(context.Context, *os.File, stationDocumentProduct) error) (stationDocumentProduct, error) {
	if in.extension != ".docx" || in.file == nil || in.check == nil || persist == nil || filename != stationPDFRuntimeName || in.handoffKey == "" {
		return stationDocumentProduct{}, errors.New("PDF 导出请求的原件或文件名无效")
	}
	args := map[string]any{"artifact_id": in.record.ID, "filename": filename}
	key := []any{"pdf-export", in.record.ID, in.record.SHA256, in.trace, in.epoch, in.requester, in.handoffKey}
	private, err := h.runStationDocument(ctx, in, projectOperation(localDocumentExportTool), args, key,
		&stationDocumentExportPolicy{filename: filename, persist: persist}, nil)
	if err != nil {
		return stationDocumentProduct{}, err
	}
	var product stationDocumentProduct
	if json.Unmarshal(private, &product) != nil || !validStationPDFProduct(product) {
		return stationDocumentProduct{}, errors.New("PDF 导出结果不完整或已损坏")
	}
	return product, nil
}

type stationDocumentExportPolicy struct {
	filename string
	persist  func(context.Context, *os.File, stationDocumentProduct) error
}

func (p stationDocumentPolicy) validatePDF(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, wire stationDocumentResultEnvelope) (pluginruntime.VerifiedResult, error) {
	var verified pluginruntime.VerifiedResult
	if len(wire.Outputs) != 1 {
		return verified, errors.New("文档工坊未返回可核对的 PDF")
	}
	var evidence struct {
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
	if json.Unmarshal(wire.Evidence, &evidence) != nil || !evidence.Verification.OK || evidence.Verification.PageCount < 1 ||
		evidence.Export.Bytes < 5 || evidence.Export.Bytes > stationDocumentLimit || !artifactID("sha-"+evidence.Export.SHA256, "sha-") || evidence.Export.SHA256 != evidence.Verification.SHA256 ||
		evidence.Export.Bytes != evidence.Verification.Bytes || evidence.Export.Backend == "" || evidence.Export.SupportedLayout == "" {
		return verified, errors.New("PDF 导出与验证回执不一致")
	}
	entry := wire.Outputs[0]
	if entry.Role != "delivery_pdf" || entry.MIMEType != "application/pdf" || entry.DisplayName != p.export.filename {
		return verified, errors.New("文档工坊输出格式不符合任务范围")
	}
	product := stationDocumentProduct{Filename: p.export.filename, SHA256: evidence.Export.SHA256, Bytes: evidence.Export.Bytes, Pages: evidence.Verification.PageCount}
	if err := p.persistVerifiedPDF(ctx, job, cap, entry.Path, product); err != nil {
		return verified, err
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return verified, err
	}
	verified.Result, _ = json.Marshal(map[string]any{"status": "prepared", "page_count": product.Pages})
	verified.Private, _ = json.Marshal(product)
	return verified, nil
}

func (p stationDocumentPolicy) persistVerifiedPDF(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, returned string, product stationDocumentProduct) error {
	rel, err := filepath.Rel(windowsProjectToolPath(p.output), windowsProjectToolPath(returned))
	if err != nil || !filepath.IsAbs(returned) || rel != product.Filename || !filepath.IsLocal(rel) || !validStationPDFName(rel) || rel != filepath.Base(rel) {
		return errors.New("PDF 输出路径不符合任务范围")
	}
	root, err := workspacefs.OpenPath(p.output)
	if err != nil {
		return errors.New("PDF 输出目录不可用")
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, rel)
	if err != nil {
		return errors.New("PDF 输出文件不可用")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != product.Bytes {
		return errors.New("PDF 输出文件大小不符合验证回执")
	}
	var header [5]byte
	if _, err := file.ReadAt(header[:], 0); err != nil || !bytes.Equal(header[:], []byte("%PDF-")) {
		return errors.New("PDF 输出文件格式不符合验证回执")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errors.New("PDF 输出文件无法校验")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, file)
	if err != nil || count != product.Bytes || hex.EncodeToString(hash.Sum(nil)) != product.SHA256 {
		return errors.New("PDF 输出文件摘要不符合验证回执")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return errors.New("PDF 输出文件无法交付")
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return err
	}
	if err := p.export.persist(ctx, file, product); err != nil {
		return errors.New("PDF 成品登记失败")
	}
	return nil
}
