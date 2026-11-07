package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func (h *localHomeService) stationDocumentAvailable(ctx context.Context, ext string) bool {
	return h.stationDocumentToolAvailable(ctx, projectOperation(localDocumentInspectTool), ext == ".pdf")
}

func (h *localHomeService) stationDocumentToolAvailable(ctx context.Context, operation localProjectOperation, needsPDF bool) bool {
	_, ok := h.stationDocumentToolRelease(ctx, operation, needsPDF)
	return ok
}

func (h *localHomeService) stationDocumentToolRelease(ctx context.Context, operation localProjectOperation, needsPDF bool) (string, bool) {
	if h.projectTools == nil || !h.projectTools.runtimeReady() || needsPDF && !h.projectTools.pdfRuntimeReady() {
		return "", false
	}
	identity, err := h.identity.Read()
	if err != nil || identity.LocalOwner == nil {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "project-tools", identity.FishID)); err != nil {
		return "", false
	}
	host, err := h.projectTools.forFish(identity.FishID)
	if err != nil {
		return "", false
	}
	installed, err := installedLocalProject(ctx, host, operation)
	return installed.Digest, err == nil && stationDocumentPermission(installed, operation.capability)
}

func stationDocumentPermission(installed pluginhost.InstalledPackage, capability string) bool {
	return slices.Contains(installed.Permissions, "artifact.read") &&
		(capability == localDocumentInspect || slices.Contains(installed.Permissions, "artifact.write"))
}

func (h *localHomeService) inspectStationDocument(ctx context.Context, in stationDocumentInput) (string, error) {
	args := map[string]any{"artifact_id": in.record.ID}
	if in.page == 0 {
		args["text_offset"], args["text_limit"] = in.offset, in.limit
	} else {
		args["pages"] = []int{in.page}
	}
	offset, limit := in.offset, in.limit
	if in.page > 0 {
		// Cache the complete verified page per request. Character continuation
		// does not run another PDF extraction or render another raster.
		in.offset, in.limit = 0, 2<<20
	}
	key := []any{in.record.ID, in.record.SHA256, in.trace, in.page, in.offset, in.limit, in.ocrMode, in.ocrDPI, in.epoch, in.requester}
	private, err := h.runStationDocument(ctx, in, projectOperation(localDocumentInspectTool), args, key, nil, nil)
	if err != nil || in.page == 0 {
		return string(private), err
	}
	return paginateStationPDF(private, offset, limit)
}

func (h *localHomeService) runStationDocument(ctx context.Context, in stationDocumentInput, operation localProjectOperation,
	args map[string]any, requestIdentity []any, export *stationDocumentExportPolicy, image *stationDocumentImagePolicy) (json.RawMessage, error) {
	capability := operation.capability
	if !h.stationDocumentToolAvailable(ctx, operation, in.extension == ".pdf" || export != nil) {
		return nil, errors.New("当前文档操作的本地文档工坊或运行环境未启用")
	}
	record, err := h.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return nil, errors.New("本机主人身份不可用")
	}
	s := h.projectTools
	host, err := s.forFish(record.FishID)
	if err != nil {
		return nil, errors.New("文档工坊存储不可用")
	}
	s.update.Lock()
	installed, err := installedLocalProject(ctx, host, operation)
	if err != nil {
		s.update.Unlock()
		return nil, err
	}
	legacy := compatibleDocumentDigest(operation, installed.Digest)
	if legacy && (in.borderlessTable || image != nil && image.layout.BorderlessTables > 0) {
		s.update.Unlock()
		return nil, errors.New("保留无边框表格需要文档工坊 0.1.6 或更高版本")
	}
	if legacy && installed.Digest != localDocumentV015Digest && (in.mergedTable || image != nil && image.layout.MergedCells > 0) {
		s.update.Unlock()
		return nil, errors.New("保留合并单元格需要文档工坊 0.1.5 或更高版本")
	}
	if image != nil && image.originalImage != nil && len(image.layout.Illustrations) > 0 &&
		legacy && installed.Digest != localDocumentV014Digest && installed.Digest != localDocumentV015Digest {
		s.update.Unlock()
		return nil, errors.New("保留插图需要文档工坊 0.1.4 或更高版本")
	}
	release := s.markActive(operation.pkg)
	s.update.Unlock()
	defer release()
	config, err := host.store.Configuration(ctx, operation.pkg)
	if err != nil {
		return nil, errors.New("文档工坊配置不可用")
	}
	source := in.record.Source
	purpose := "inspect authorized incoming WeChat document"
	if export != nil {
		purpose = "export authorized incoming WeChat document to PDF"
	} else if capability == localDocumentRevise {
		purpose = "resize title of authorized WeChat document"
	} else if image != nil {
		purpose = "reconstruct authorized incoming WeChat image as editable document"
	}
	invocation := pluginhost.Invocation{
		Account: record.FishID, Requester: record.LocalOwner.ID, Purpose: purpose,
		Thread:         localCheckpointKey("wechat-document", source.Account, source.Dataset, source.Thread),
		Authorization:  localCheckpointKey(record.LocalOwner.ID, record.LocalOwner.Revision, source.Grant, in.requester, in.epoch),
		RequestKey:     localCheckpointKey(append(requestIdentity, record.LocalOwner.Revision, installed.Revision, config.Revision)...),
		SourceVersions: in.versions,
	}
	check := func(ctx context.Context, job pluginhost.Job) error {
		if err := in.check(ctx); err != nil {
			return err
		}
		identity, err := h.identity.Read()
		if err != nil || identity.FishID != record.FishID || identity.LocalOwner == nil ||
			identity.LocalOwner.ID != record.LocalOwner.ID || identity.LocalOwner.Revision != record.LocalOwner.Revision {
			return errors.New("本机主人授权已变化")
		}
		current, err := installedLocalProject(ctx, host, operation)
		if err != nil || current.Revision != installed.Revision || !stationDocumentPermission(current, capability) {
			return errors.New("文档工坊版本或授权已变化")
		}
		cfg, err := host.store.Configuration(ctx, operation.pkg)
		if err != nil || cfg.Revision != config.Revision || job.Plugin != operation.pkg || job.Tool != capability ||
			job.PackageDigest != installed.Digest || job.Version != installed.Package.Version ||
			job.ConfigRevision != strconv.Itoa(config.Revision) || !reflect.DeepEqual(job.Context, invocation) {
			return errors.New("文档工坊任务或配置已变化")
		}
		if job.ID != "" {
			fresh, err := host.store.Job(ctx, job.ID)
			if err != nil || fresh.CancelRequested || fresh.Revision != job.Revision {
				return errors.New("文档任务已取消或状态已变化")
			}
		}
		return nil
	}
	options := s.options
	if in.extension == ".docx" && export == nil {
		options.DocumentRuntimeRoot = ""
	}
	controller, input, output, cleanup, err := openScopedProjectRuntime(ctx, filepath.Join(h.dataDir, "project-tool-tasks"),
		host.store, options, operation.pkg, true, func(ctx context.Context, dir string) (localProjectArtifactMaterialized, error) {
			material, err := in.materialize(ctx, dir)
			if err == nil && image != nil && image.originalImage != nil && len(image.layout.Illustrations) != 0 {
				err = image.stageIllustrations(ctx, dir, image.originalImage)
				if err == nil {
					err = in.check(ctx)
				}
			}
			return material, err
		})
	if err != nil {
		return nil, errors.New("无法创建文档任务所需的隔离输入")
	}
	defer cleanup()
	policy := stationDocumentPolicy{in: in, input: input, output: output, capability: capability, check: check, export: export, image: image}
	ocrSpec := func(ctx context.Context) (pluginruntime.LaunchSpec, error) {
		executor := pluginruntime.InstalledExecutor(host.store, options)
		grant := pluginhost.Grant{Account: record.FishID, Plugin: operation.pkg, Version: installed.Package.Version, Permissions: installed.Permissions}
		spec, err := executor.Resolve(ctx, installed.Package, grant)
		spec.DataDir = output
		spec.ReadOnlyDirs = append(spec.ReadOnlyDirs, filepath.Dir(input))
		spec.ReadOnlyFiles = append(spec.ReadOnlyFiles, input)
		return spec, err
	}
	if in.page > 0 && (in.ocrMode == "auto" || in.ocrMode == "always") {
		policy.scanPDF = func(ctx context.Context) (pluginruntime.DocumentOCRObservation, error) {
			spec, err := ocrSpec(ctx)
			if err != nil {
				return pluginruntime.DocumentOCRObservation{}, errors.New("pdf_scan_unavailable")
			}
			return pluginruntime.RunDocumentPDFOCR(ctx, spec, pluginruntime.DocumentPDFOCRRequest{
				RuntimeRoot: options.DocumentRuntimeRoot, InputPath: input, InputSHA256: in.record.SHA256, Page: in.page, DPI: in.ocrDPI,
			})
		}
	}
	if image != nil && image.inspect {
		// OCR is the initial LPAC process. Only its hash-bound observation
		// goes to the document workshop through task metadata.
		spec, resolveErr := ocrSpec(ctx)
		if resolveErr != nil {
			return nil, errors.New("文档 OCR 隔离运行器不可用")
		}
		observation, ocrErr := pluginruntime.RunDocumentOCR(ctx, spec, pluginruntime.DocumentOCRRequest{
			RuntimeRoot: options.DocumentRuntimeRoot, InputPath: input, InputSHA256: in.record.SHA256,
		})
		if ocrErr != nil {
			return nil, ocrErr
		}
		policy.image.ocrObservation = &observation
	}
	if err := controller.EnsureStarted(ctx, operation.pkg, installed.Revision); err != nil {
		return nil, errors.New("文档工坊隔离运行器未能启动")
	}
	raw, _ := json.Marshal(args)
	result, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: operation.pkg, Capability: capability, Context: invocation, Arguments: raw}, policy)
	if err != nil {
		return nil, err
	}
	job := result.Job
	if err := check(ctx, job); err != nil {
		return nil, err
	}
	if job.State == "failed" {
		var reason struct{ Reason string }
		if json.Unmarshal(job.Result, &reason) == nil && pluginruntime.SafeDocumentResultCode(reason.Reason) {
			if reason.Reason == "document_title_missing" {
				return nil, errors.New("这份 Word 没有标记为主标题的段落，请先确认标题")
			}
			return nil, errors.New("文档工坊处理失败（" + reason.Reason + "）")
		}
	}
	if job.State != "completed" {
		return nil, errors.New("文档处理未得到可核对的结果")
	}
	return host.store.PrivateJobResult(ctx, job, check)
}

func (in stationDocumentInput) materialize(ctx context.Context, dir string) (localProjectArtifactMaterialized, error) {
	if err := in.check(ctx); err != nil {
		return localProjectArtifactMaterialized{}, err
	}
	path := filepath.Join(dir, "input"+in.extension)
	err := stageStationMedia(ctx, path, in.file, in.record)
	if err == nil {
		err = in.check(ctx)
	}
	if err != nil {
		_ = os.Remove(path)
		return localProjectArtifactMaterialized{}, err
	}
	return localProjectArtifactMaterialized{InputPath: path}, nil
}

type stationDocumentPolicy struct {
	in            stationDocumentInput
	input, output string
	capability    string
	check         func(context.Context, pluginhost.Job) error
	export        *stationDocumentExportPolicy
	image         *stationDocumentImagePolicy
	scanPDF       func(context.Context) (pluginruntime.DocumentOCRObservation, error)
}

func (p stationDocumentPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) error {
	if cap.ID != p.capability || cap.Async {
		return errors.New("文档检查能力与任务不匹配")
	}
	return p.check(ctx, job)
}

func (p stationDocumentPolicy) CallMetadata(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) (map[string]json.RawMessage, error) {
	if err := p.Check(ctx, job, cap); err != nil {
		return nil, err
	}
	mimeType := localDOCXMIME
	if p.in.extension == ".pdf" {
		mimeType = "application/pdf"
	} else if p.in.extension == ".png" || p.in.extension == ".jpg" {
		// The host assigned this MIME only after DecodeConfig verified the
		// original bytes. Never trust a user-provided extension here.
		mimeType = p.in.record.MIME
	}
	out := windowsProjectToolPath(p.output)
	inputs := []map[string]string{{"artifact_id": p.in.record.ID,
		"path": windowsProjectToolPath(p.input), "sha256": p.in.record.SHA256, "mime_type": mimeType}}
	if p.image != nil {
		inputs = append(inputs, p.image.illustrationInputs...)
	}
	metadata := map[string]any{"inputs": inputs,
		"input_root": windowsProjectToolPath(filepath.Dir(p.input)), "output_dir": out, "output_root": out}
	if p.image != nil && p.image.inspect {
		if p.image.ocrObservation == nil {
			return nil, errors.New("图片 OCR 结果不可用")
		}
		metadata["ocr_observation"] = p.image.ocrObservation
	}
	raw, err := json.Marshal(metadata)
	return map[string]json.RawMessage{"wechat/document": raw}, err
}

type stationDocumentResultEnvelope struct {
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

func (p stationDocumentPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	var result pluginruntime.VerifiedResult
	if err := p.Check(ctx, job, cap); err != nil {
		return result, err
	}
	var wire stationDocumentResultEnvelope
	if len(raw) > 2<<20 || json.Unmarshal(raw, &wire) != nil {
		return result, errors.New("文档工坊返回格式无效")
	}
	if !wire.OK && pluginruntime.SafeDocumentResultCode(wire.Error) && len(wire.Outputs) == 0 {
		result.Failed = true
		result.Result, _ = json.Marshal(map[string]string{"reason": wire.Error})
		return result, nil
	}
	if !wire.OK {
		return result, errors.New("文档工坊未返回可核验的结果")
	}
	if p.export != nil {
		return p.validatePDF(ctx, job, cap, wire)
	}
	if p.image != nil {
		if p.image.reconstruct {
			return p.validatePreparedDOCX(ctx, job, cap, wire)
		}
		if len(wire.Outputs) != 0 {
			return result, errors.New("图片识别返回了意外成品")
		}
		if err := stationImageInspectionEvidence(p.in, wire.Evidence); err != nil {
			return result, err
		}
		result.Result = json.RawMessage(`{"status":"inspected"}`)
		result.Private = append(json.RawMessage(nil), wire.Evidence...)
		return result, nil
	}
	if len(wire.Outputs) != 0 {
		return result, errors.New("文档检查返回了意外成品")
	}
	content, err := stationDocumentResult(p.in, wire.Evidence)
	if err != nil {
		return result, err
	}
	if p.in.page > 0 {
		if err := p.applyPDFScan(ctx, job, cap, content); err != nil {
			return result, err
		}
	}
	result.Result = json.RawMessage(`{"status":"inspected"}`)
	result.Private, err = json.Marshal(content)
	return result, err
}

func stationDocumentResult(in stationDocumentInput, evidence json.RawMessage) (map[string]any, error) {
	var result struct {
		Kind      string `json:"kind"`
		SHA256    string `json:"sha256"`
		Bytes     int64  `json:"bytes"`
		Text      string `json:"text"`
		Total     *int   `json:"text_total_characters"`
		Offset    *int   `json:"text_offset"`
		Pages     []int  `json:"pages"`
		PageCount int    `json:"page_count"`
	}
	mismatch := errors.New("文档检查结果的格式、来源或读取范围不匹配")
	if json.Unmarshal(evidence, &result) != nil || result.Kind != strings.TrimPrefix(in.extension, ".") {
		return nil, mismatch
	}
	source := in.record.Source
	view := map[string]any{"status": "inspected", "format": result.Kind, "message_id": source.Message,
		"asset_id": in.record.ID, "sha256": in.record.SHA256, "filename": source.Filename,
		"sender_id": source.Sender, "occurred_at": source.Occurred, "untrusted_document_content": true}
	text := []rune(result.Text)
	total := 0
	if in.page == 0 {
		if result.SHA256 != in.record.SHA256 || result.Bytes != in.record.Bytes || result.Total == nil || result.Offset == nil ||
			*result.Offset != in.offset || *result.Total < in.offset || len(text) != min(in.limit, *result.Total-in.offset) {
			return nil, mismatch
		}
		total = *result.Total
	} else {
		if !slices.Equal(result.Pages, []int{in.page}) || result.PageCount < in.page {
			return nil, mismatch
		}
		total = len(text)
		if in.offset > total {
			return nil, errors.New("正文偏移超过该 PDF 页的文字长度")
		}
		text = text[in.offset : in.offset+min(in.limit, total-in.offset)]
		view["page"], view["page_count"] = in.page, result.PageCount
		if in.offset+len(text) == total && in.page < result.PageCount {
			view["next_page"] = in.page + 1
		}
	}
	if total == 0 {
		view["status"] = "no_extractable_text"
	}
	view["text"], view["text_offset"], view["text_total_characters"] = string(text), in.offset, total
	if in.offset+len(text) < total {
		view["next_text_offset"] = in.offset + len(text)
	}
	return view, nil
}
