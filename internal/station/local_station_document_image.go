package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
)

// stationDocumentImageAvailable deliberately checks both the document worker
// and its reviewed runtime. Image reconstruction depends on the host-owned
// initial OCR process, so an ordinary DOCX-only installation is not enough.
func (h *localHomeService) stationDocumentImageAvailable(ctx context.Context) bool {
	return runtime.GOOS == "windows" && h.projectTools != nil && h.projectTools.runtimeReady() &&
		stationImageOCRRuntimeReady(h.projectTools.options.DocumentRuntimeRoot) &&
		h.stationDocumentToolAvailable(ctx, projectOperation(localDocumentInspectTool), false) &&
		h.stationDocumentToolAvailable(ctx, projectOperation(localDocumentToolName), false)
}

func (h *localHomeService) stationDocumentIllustrationsAvailable(ctx context.Context) bool {
	operation := projectOperation(localDocumentToolName)
	digest, ok := h.stationDocumentToolRelease(ctx, operation, false)
	return ok && (digest == localDocumentV014Digest || digest == localDocumentV015Digest || !compatibleDocumentDigest(operation, digest))
}

func stationImageOCRRuntimeReady(path string) bool {
	if path == "" {
		return false
	}
	root, err := workspacefs.OpenPath(path)
	if err != nil {
		return false
	}
	defer root.Close()
	manifest, err := workspacefs.OpenRegular(root, "runtime-manifest.json")
	if err != nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(manifest, 1<<20))
	_ = manifest.Close()
	if err != nil || len(raw) > 1<<20 {
		return false
	}
	var value struct {
		Components map[string]any `json:"components"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Components["tesseract_chi_sim"] != true {
		return false
	}
	for _, name := range []string{"tesseract/tesseract.exe", "tesseract/tessdata/chi_sim.traineddata"} {
		file, fileErr := workspacefs.OpenRegular(root, name)
		if fileErr != nil {
			return false
		}
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() < 1 {
			return false
		}
	}
	return true
}

// reconstructStationDocumentImage turns one already-authorized image into a
// native DOCX. There is no model decision in this path: the workshop receives
// a hash-bound OCR observation, Go converts its measured layout into Word
// blocks, and the result is accepted only after its actual file is verified.
func (h *localHomeService) reconstructStationDocumentImage(ctx context.Context, in stationDocumentInput,
	persist func(context.Context, *os.File, stationDocumentProduct, stationImageLayout) error) (stationImageLayout, error) {
	var empty stationImageLayout
	if in.file == nil || in.check == nil || persist == nil || in.handoffKey == "" ||
		(in.extension != ".png" && in.extension != ".jpg") ||
		(in.record.MIME != "image/png" && in.record.MIME != "image/jpeg") {
		return empty, errors.New("图片文档复刻请求的原件或格式无效")
	}
	if err := in.check(ctx); err != nil {
		return empty, err
	}
	// OCR may use a task-private, rule-cleaned derivative so that a ruled
	// table does not hide its cell text from Tesseract. The source identity and
	// later DOCX reconstruction remain bound to the original attachment.
	ocrInput, preparation, cleanupOCR, err := stationPrepareImageOCRInput(ctx, in, h.dataDir)
	if err != nil {
		return empty, err
	}
	defer cleanupOCR()
	if preparation.grid.Ambiguous {
		return markImageLayoutReview(stationImageLayout{ImageWidth: preparation.original.Bounds().Dx(), ImageHeight: preparation.original.Bounds().Dy()},
			"图片表格边界不完整或相互冲突，请核对原图")
	}
	inspect := &stationDocumentImagePolicy{inspect: true}
	inspectArgs := map[string]any{"artifact_id": in.record.ID, "include_layout": true}
	inspectKey := []any{"image-ocr-layout", in.record.ID, in.record.SHA256, in.trace, in.epoch, in.requester, in.handoffKey}
	evidence, err := h.runStationDocument(ctx, ocrInput, projectOperation(localDocumentInspectTool), inspectArgs, inspectKey, nil, inspect)
	if err != nil {
		return empty, err
	}
	// The deterministic layout parser receives the initial OCR evidence and a
	// host-produced table grid. Neither contains a path supplied by a message.
	layout, err := stationImageWordLayout(evidence, preparation.grid, preparation.regions...)
	if err != nil {
		return empty, err
	}
	layout.ImageWidth, layout.ImageHeight = preparation.original.Bounds().Dx(), preparation.original.Bounds().Dy()
	if layout.NeedsClarification {
		// A low-confidence original is useful diagnostic state but must never be
		// converted into a plausible-looking, wrong editable document.
		return layout, nil
	}
	if len(layout.Layout) == 0 {
		return empty, errors.New("图片识别未产生可核对的 Word 版式")
	}
	if _, err := in.file.Seek(0, io.SeekStart); err != nil {
		return empty, errors.New("图片原件无法重新读取")
	}
	reconstruct := &stationDocumentImagePolicy{reconstruct: true, filename: stationDOCXRuntimeName, layout: layout, persist: persist, originalImage: preparation.original}
	// document.reconstruct accepts only a measured layout and a fixed output
	// filename. Its input remains host metadata; passing artifact_id here would
	// be rejected by the plugin's strict argument schema.
	reconstructArgs := map[string]any{"layout": json.RawMessage(layout.Layout), "filename": stationDOCXRuntimeName}
	if len(layout.Illustrations) != 0 {
		images := make([]map[string]string, len(layout.Illustrations))
		for i := range images {
			images[i] = map[string]string{"artifact_id": stationIllustrationID(i)}
		}
		reconstructArgs["images"] = images
	}
	reconstructKey := []any{"image-docx-reconstruct", in.record.ID, in.record.SHA256, in.trace, in.epoch, in.requester, in.handoffKey, json.RawMessage(layout.Layout)}
	private, err := h.runStationDocument(ctx, in, projectOperation(localDocumentToolName), reconstructArgs, reconstructKey, nil, reconstruct)
	if err != nil {
		return empty, err
	}
	var product stationDocumentProduct
	if json.Unmarshal(private, &product) != nil || !validStationDocumentProduct(product, ".docx") {
		return empty, errors.New("图片复刻的 Word 成品不完整或已损坏")
	}
	return layout, nil
}

func (h *localHomeService) stationDocumentRevisionAvailable(ctx context.Context) bool {
	return h.stationDocumentToolAvailable(ctx, projectOperation(localDocumentReviseTool), false)
}

func (h *localHomeService) reviseStationDocumentTitle(ctx context.Context, in stationDocumentInput, halfPoints int,
	persist func(context.Context, *os.File, stationDocumentProduct) error) error {
	if in.extension != ".docx" || in.file == nil || in.check == nil || persist == nil || in.handoffKey == "" || halfPoints < 16 || halfPoints > 96 {
		return errors.New("Word 修订缺少已核验原稿或有效字号")
	}
	policy := &stationDocumentImagePolicy{reconstruct: true, filename: stationDOCXRuntimeName, titleHalfPoints: halfPoints,
		layout: stationImageLayout{Illustrations: in.illustrationRegions},
		persist: func(ctx context.Context, f *os.File, p stationDocumentProduct, _ stationImageLayout) error {
			return persist(ctx, f, p)
		}}
	args := map[string]any{"artifact_id": in.record.ID, "filename": stationDOCXRuntimeName, "font_size_pt": float64(halfPoints) / 2}
	key := []any{"docx-title-revision", in.record.ID, in.record.SHA256, in.handoffKey}
	private, err := h.runStationDocument(ctx, in, projectOperation(localDocumentReviseTool), args, key, nil, policy)
	if err != nil {
		return err
	}
	var product stationDocumentProduct
	if json.Unmarshal(private, &product) != nil || !validStationDocumentProduct(product, ".docx") {
		return errors.New("Word 修订结果不完整")
	}
	return nil
}

type stationDocumentImagePolicy struct {
	inspect, reconstruct bool
	filename             string
	layout               stationImageLayout
	persist              func(context.Context, *os.File, stationDocumentProduct, stationImageLayout) error
	ocrObservation       *pluginruntime.DocumentOCRObservation
	titleHalfPoints      int
	originalImage        image.Image
	illustrationInputs   []map[string]string
}

func stationImageInspectionEvidence(in stationDocumentInput, evidence json.RawMessage) error {
	var result struct {
		OK        bool   `json:"ok"`
		Kind      string `json:"kind"`
		PageCount int    `json:"page_count"`
		Pages     []int  `json:"pages"`
		OCR       struct {
			Available bool   `json:"available"`
			Attempted bool   `json:"attempted"`
			Engine    string `json:"engine"`
		} `json:"ocr"`
	}
	if json.Unmarshal(evidence, &result) != nil || !result.OK || result.Kind != "image" || result.PageCount != 1 ||
		len(result.Pages) != 1 || result.Pages[0] != 1 || !result.OCR.Available || !result.OCR.Attempted ||
		result.OCR.Engine != "host_initial_lpac_tesseract" || (in.extension != ".png" && in.extension != ".jpg") {
		return errors.New("图片 OCR 结果与受控原件不匹配")
	}
	return nil
}

func (p stationDocumentPolicy) validatePreparedDOCX(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, wire stationDocumentResultEnvelope) (pluginruntime.VerifiedResult, error) {
	var verified pluginruntime.VerifiedResult
	if p.image == nil || !p.image.reconstruct || p.image.persist == nil || p.image.filename != stationDOCXRuntimeName ||
		len(wire.Outputs) != 1 {
		return verified, errors.New("Word 成品未返回可核对的成品")
	}
	var evidence struct {
		SHA256      string  `json:"sha256"`
		InputSHA256 string  `json:"input_sha256"`
		FontSize    float64 `json:"font_size_pt"`
	}
	if json.Unmarshal(wire.Evidence, &evidence) != nil || !artifactID("sha-"+evidence.SHA256, "sha-") {
		return verified, errors.New("Word 成品回执无效")
	}
	if p.image.titleHalfPoints != 0 && (evidence.InputSHA256 != p.in.record.SHA256 || evidence.FontSize*2 != float64(p.image.titleHalfPoints)) {
		return verified, errors.New("Word 修订回执的原稿摘要或字号不匹配")
	}
	entry := wire.Outputs[0]
	if entry.Role != "editable_source" || entry.MIMEType != localDOCXMIME || entry.DisplayName != p.image.filename {
		return verified, errors.New("Word 成品输出格式不符合任务范围")
	}
	product, err := p.persistVerifiedDOCX(ctx, job, cap, entry.Path, evidence.SHA256)
	if err != nil {
		return verified, err
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return verified, err
	}
	verified.Result, _ = json.Marshal(map[string]any{"status": "prepared", "format": "docx"})
	verified.Private, _ = json.Marshal(product)
	return verified, nil
}

func (p stationDocumentPolicy) persistVerifiedDOCX(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, returned, evidenceSHA string) (stationDocumentProduct, error) {
	var empty stationDocumentProduct
	if p.image == nil || p.image.persist == nil {
		return empty, errors.New("Word 成品登记不可用")
	}
	rel, err := filepath.Rel(windowsProjectToolPath(p.output), windowsProjectToolPath(returned))
	if err != nil || !filepath.IsAbs(returned) || rel != p.image.filename || !filepath.IsLocal(rel) ||
		!validStationDocumentName(rel, ".docx") || rel != filepath.Base(rel) {
		return empty, errors.New("Word 输出路径不符合任务范围")
	}
	root, err := workspacefs.OpenPath(p.output)
	if err != nil {
		return empty, errors.New("Word 输出目录不可用")
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, rel)
	if err != nil {
		return empty, errors.New("Word 输出文件不可用")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > stationDocumentLimit ||
		validateLocalDOCX(file, info.Size()) != nil {
		return empty, errors.New("Word 输出文件结构无效")
	}
	if err := stationVerifyWordIllustrations(file, info.Size(), p.image.layout.Illustrations); err != nil {
		return empty, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return empty, errors.New("Word 输出文件无法校验")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	actualSHA := hex.EncodeToString(hash.Sum(nil))
	if err != nil || size != info.Size() || actualSHA != evidenceSHA {
		return empty, errors.New("Word 输出文件摘要不符合验证回执")
	}
	product := stationDocumentProduct{Filename: p.image.filename, SHA256: actualSHA, Bytes: size}
	if !validStationDocumentProduct(product, ".docx") {
		return empty, errors.New("Word 成品信息无效")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return empty, errors.New("Word 输出文件无法交付")
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return empty, err
	}
	if err := p.image.persist(ctx, file, product, p.image.layout); err != nil {
		return empty, errors.New("Word 成品登记失败")
	}
	return product, nil
}
