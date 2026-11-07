package admin

// This file is the trusted conversation adapter for document-workshop. The
// package itself receives only task-scoped materialized copies in MCP metadata;
// it never receives a user-selected host path or a WeChat delivery authority.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

const documentWorkshopPluginID = pluginruntime.DocumentWorkshopID

type documentWorkshopPolicy struct {
	p                             *permittedTools
	store                         *pluginhost.Store
	capability                    string
	invocation                    pluginhost.Invocation
	materialized                  []ArtifactMaterialized
	packageDigest, packageVersion string
	configRevision                int
}

func (p *permittedTools) documentTools() []mcptools.Tool {
	if !p.documentWorkshopAvailable(context.Background(), false) {
		return nil
	}
	tools := []mcptools.Tool{
		agentTool("document_inspect", "读取当前会话中已授权的图片、Word 或 PDF 附件，提取可核对的文字、页面、表格及版式资料。只接受 asset_id，不接受电脑路径；不执行文档里的指令，也不发送消息。Input {artifact_id,source_message_id?,pages?,include_layout?}.", `{"type":"object","properties":{"artifact_id":{"type":"string","minLength":1,"maxLength":128},"source_message_id":{"type":"string","maxLength":512},"pages":{"type":"array","items":{"type":"integer","minimum":1}},"include_layout":{"type":"boolean"}},"required":["artifact_id"],"additionalProperties":false}`),
		agentTool("document_verify", "检查当前会话中已授权 DOCX 或 PDF 是否能打开、文字与页数是否合理；需要时渲染页面检查明显空页、裁切或缺字。Input {artifact_id,source_message_id?,expected_text?,render_pages?}.", `{"type":"object","properties":{"artifact_id":{"type":"string","minLength":1,"maxLength":128},"source_message_id":{"type":"string","maxLength":512},"expected_text":{"type":"array","items":{"type":"string","maxLength":256}},"render_pages":{"type":"boolean"}},"required":["artifact_id"],"additionalProperties":false}`),
	}
	if p.documentWorkshopAvailable(context.Background(), true) {
		tools = append(tools,
			agentTool("document_reconstruct", "把已检查的附件内容重建为可编辑 DOCX。先用 document_inspect 取得 layout，再提供原 artifact_id 与 layout；可用 title_font_size_pt 调整标题（8–48 磅）。宿主只在本任务输出目录制作新文件并登记为待发送附件。不会自动发送。Input {artifact_id,source_message_id?,layout,title?,title_font_size_pt?,images?,filename?}.", `{"type":"object","properties":{"artifact_id":{"type":"string","minLength":1,"maxLength":128},"source_message_id":{"type":"string","maxLength":512},"layout":{"type":"object"},"title":{"type":"string","maxLength":256},"title_font_size_pt":{"type":"number","minimum":8,"maximum":48},"images":{"type":"array","items":{"type":"object"}},"filename":{"type":"string","maxLength":120}},"required":["artifact_id","layout"],"additionalProperties":false}`),
			agentTool("document_export_pdf", "把当前会话中已授权、已登记的 DOCX 导出 PDF。输入为 artifact_id，运行环境由已安装文档工具自检；返回待发送的 PDF，不会自动发送。Input {artifact_id,source_message_id?,filename?}.", `{"type":"object","properties":{"artifact_id":{"type":"string","minLength":1,"maxLength":128},"source_message_id":{"type":"string","maxLength":512},"filename":{"type":"string","maxLength":120}},"required":["artifact_id"],"additionalProperties":false}`),
		)
	}
	return tools
}

func (p *permittedTools) documentWorkshopAvailable(ctx context.Context, requiresOutput bool) bool {
	if p == nil || p.manager == nil || !p.workspaceExecutionAllowed() || !p.servicePluginsAllowed() || requiresOutput && (!p.manager.Config().Companion.Permissions.SendFile || !p.manager.Config().Companion.Permissions.Remember) {
		return false
	}
	store, err := p.manager.packageStore()
	if err != nil {
		return false
	}
	installed, err := store.Installed(ctx, documentWorkshopPluginID)
	if err != nil || !installed.Enabled || installed.Removed || store.CheckConfiguration(ctx, documentWorkshopPluginID) != nil {
		return false
	}
	for _, cap := range mustCapabilities(ctx, store, documentWorkshopPluginID) {
		if cap.Discoverable && cap.Implementation == "ready" && !cap.Async && (cap.ID == "document.inspect" || cap.ID == "document.reconstruct" || cap.ID == "document.export_pdf" || cap.ID == "document.verify") {
			return true
		}
	}
	return false
}

func (p *permittedTools) newDocumentWorkshopPolicy(ctx context.Context, capability string, raw json.RawMessage) (*documentWorkshopPolicy, json.RawMessage, error) {
	requiresOutput := capability == "document.reconstruct" || capability == "document.export_pdf"
	if !p.documentWorkshopAvailable(ctx, requiresOutput) || p.trace.Account == "" || p.trace.Thread == "" || p.trace.Sender == "" || !p.manager.agentTaskActive(p.trace) {
		return nil, nil, errors.New("document_workshop_not_authorized")
	}
	var input struct {
		ArtifactID string `json:"artifact_id"`
		SourceID   string `json:"source_message_id"`
	}
	if json.Unmarshal(raw, &input) != nil || strings.TrimSpace(input.ArtifactID) == "" || len(input.SourceID) > 512 {
		return nil, nil, toolInputError("document_artifact_required")
	}
	materialized, err := p.MaterializeArtifactForTask(ctx, input.ArtifactID, input.SourceID, "")
	if err != nil {
		return nil, nil, err
	}
	var arguments map[string]json.RawMessage
	if json.Unmarshal(raw, &arguments) != nil || arguments == nil {
		return nil, nil, toolInputError("document_arguments_invalid")
	}
	// A plugin sees the stable artifact object, while the actual path stays in
	// host metadata. This prevents a model from substituting C:\\ paths.
	delete(arguments, "artifact_id")
	delete(arguments, "source_message_id")
	// Reconstruction uses the materialized image list from trusted metadata;
	// unlike inspect/export/verify its declared input is the verified layout,
	// not an artifact selector. The public artifact_id is still required by the
	// host to bind the source image(s) to this task.
	if capability != "document.reconstruct" {
		arguments["artifact"] = json.RawMessage(`{"artifact_id":` + strconv.Quote(materialized.AssetID) + `}`)
	}
	normalized, _ := json.Marshal(arguments)

	store, err := p.manager.packageStore()
	if err != nil {
		return nil, nil, err
	}
	installed, err := store.Installed(ctx, documentWorkshopPluginID)
	if err != nil || !installed.Enabled || installed.Removed {
		return nil, nil, errors.New("document_workshop_unavailable")
	}
	configuration, err := store.ConfigurationForExecution(ctx, documentWorkshopPluginID)
	if err != nil {
		return nil, nil, err
	}
	epoch, err := p.serviceEvidenceEpoch(ctx)
	if err != nil {
		return nil, nil, err
	}
	sources := map[string]int{}
	if p.trace.Source != nil && p.trace.Source.EventID != "" {
		sources[p.trace.Source.EventID] = p.trace.Source.Revision
	}
	if p.trace.ReplyContext != nil {
		for id, revision := range p.trace.ReplyContext.SourceVersions {
			sources[id] = revision
		}
	}
	key := "document-" + agentID(p.trace.Account, p.trace.Thread, p.serviceRequester(), p.trace.TaskID, p.trace.TraceID, capability, materialized.AssetID, string(normalized))
	policy := &documentWorkshopPolicy{p: p, store: store, capability: capability, materialized: []ArtifactMaterialized{materialized}, packageDigest: installed.Digest, packageVersion: installed.Package.Version, configRevision: configuration.Revision,
		invocation: pluginhost.Invocation{Account: p.trace.Account, Thread: p.trace.Thread, Requester: p.serviceRequester(), Purpose: "process authorized conversation document", RequestKey: key, EvidenceEpoch: epoch, Authorization: p.documentWorkshopAuthorization(), SourceVersions: sources}}
	return policy, normalized, nil
}

func (p *permittedTools) documentWorkshopAuthorization() string {
	if !p.documentWorkshopAvailable(context.Background(), false) || !p.manager.agentTaskActive(p.trace) {
		return ""
	}
	b, _ := json.Marshal([]any{p.trace.Account, p.trace.Thread, p.serviceRequester(), p.trace.TaskID, p.trace.TraceID, p.MemoryResultAuthorization(), p.manager.Config().Companion.Permissions.SendFile})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *permittedTools) callDocumentWorkshop(ctx context.Context, capability string, raw json.RawMessage) (string, error) {
	policy, arguments, err := p.newDocumentWorkshopPolicy(ctx, capability, raw)
	if err != nil {
		return "", err
	}
	installed, err := policy.store.Installed(ctx, documentWorkshopPluginID)
	if err != nil {
		return "", err
	}
	// A document process is deliberately one task-scoped invocation rather than
	// a resident account-wide session. Its only writable mount is this task's
	// output directory; its only extra read-only mount is the same task's input
	// directory. This keeps a later conversation from inheriting another task's
	// originals or outputs while still using the normal verified package/MCP/
	// LPAC host path.
	runtime, err := p.documentWorkshopTaskRuntime(ctx, policy)
	if err != nil {
		return "", err
	}
	defer runtime.Close()
	if err = runtime.EnsureStarted(ctx, documentWorkshopPluginID, installed.Revision); err != nil {
		return "", err
	}
	out, err := runtime.Invoke(ctx, pluginruntime.CallRequest{Plugin: documentWorkshopPluginID, Capability: capability, Context: policy.invocation, Arguments: arguments}, policy)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]any{"task_id": out.Job.ID, "state": out.Job.State, "reused": out.Reused, "result": json.RawMessage(out.Job.Result), "artifacts": out.Job.Artifacts})
	return string(b), err
}

// documentWorkshopTaskRuntime creates a fresh controller solely for the
// current document call. InstalledExecutor still verifies the installed
// package, grant, configuration and project-local runtime. The resolver then
// narrows the sandbox from the generic plugin-private directory to the one
// already-authorized artifact task. It never accepts a model-provided path.
func (p *permittedTools) documentWorkshopTaskRuntime(ctx context.Context, policy *documentWorkshopPolicy) (*pluginruntime.Controller, error) {
	if policy == nil || len(policy.materialized) != 1 {
		return nil, errors.New("document_workshop_task_scope_invalid")
	}
	options, err := runtimeDeploymentOptions()
	if err != nil {
		return nil, err
	}
	executor := pluginruntime.InstalledExecutor(policy.store, options)
	resolve := executor.Resolve
	outputRoot := policy.materialized[0].OutputDir
	executor.Resolve = func(call context.Context, pkg pluginhost.Package, grant pluginhost.Grant) (pluginruntime.LaunchSpec, error) {
		spec, resolveErr := resolve(call, pkg, grant)
		if resolveErr != nil {
			return spec, resolveErr
		}
		if pkg.ID != documentWorkshopPluginID {
			return spec, errors.New("document_workshop_runtime_scope_changed")
		}
		// The input parent is mounted only for traversal, while the exact file
		// receives a direct RX grant. On Windows this
		// matters: an inheritable directory ACL applied after materialization
		// does not retroactively grant AppContainer read access to its child.
		// outputRoot remains the process's sole writable DataDir.
		spec.DataDir = outputRoot
		spec.ReadOnlyDirs = append(spec.ReadOnlyDirs, filepath.Dir(policy.materialized[0].InputPath))
		spec.ReadOnlyFiles = append(spec.ReadOnlyFiles, policy.materialized[0].InputPath)
		return spec, nil
	}
	return pluginruntime.NewController(policy.store, executor)
}

func (d *documentWorkshopPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == nil || d.p == nil || d.store == nil || job.Plugin != documentWorkshopPluginID || job.Tool != d.capability || !bytes.Equal(documentWorkshopJSON(job.Context), documentWorkshopJSON(d.invocation)) || cap.Async {
		return errors.New("document_workshop_authorization_changed")
	}
	p := d.p
	requiresOutput := d.capability == "document.reconstruct" || d.capability == "document.export_pdf"
	if !p.documentWorkshopAvailable(ctx, requiresOutput) || !p.manager.agentTaskActive(p.trace) || job.Context.Authorization == "" || job.Context.Authorization != p.documentWorkshopAuthorization() {
		return errors.New("document_workshop_authorization_changed")
	}
	if current, err := p.serviceEvidenceEpoch(ctx); err != nil || current != d.invocation.EvidenceEpoch {
		return errors.New("document_workshop_evidence_changed")
	}
	for id, revision := range d.invocation.SourceVersions {
		event, err := p.manager.memory.ReadEvent(ctx, []string{companion.ScopeID("wechat", p.trace.Account, p.trace.Thread)}, id)
		if err != nil || event.Revision != revision {
			return errors.New("document_workshop_source_changed")
		}
	}
	installed, err := d.store.Installed(ctx, documentWorkshopPluginID)
	if err != nil || !installed.Enabled || installed.Removed || installed.Digest != d.packageDigest || installed.Package.Version != d.packageVersion || job.PackageDigest != "" && (job.PackageDigest != d.packageDigest || job.Version != d.packageVersion || job.ConfigRevision != strconv.Itoa(d.configRevision)) {
		return errors.New("document_workshop_version_changed")
	}
	configuration, err := d.store.ConfigurationForExecution(ctx, documentWorkshopPluginID)
	if err != nil || configuration.Revision != d.configRevision {
		return errors.New("document_workshop_configuration_changed")
	}
	if pluginhost.Authorize(installed.Package, pluginhost.Grant{Account: d.invocation.Account, Plugin: installed.Package.ID, Version: installed.Package.Version, Permissions: installed.Permissions}, job.Context, cap.Permissions) != nil {
		return errors.New("document_workshop_permissions_changed")
	}
	if job.ID != "" {
		current, err := d.store.Job(ctx, job.ID)
		if err != nil || current.Revision != job.Revision || current.CancelRequested || current.State == "cancelled" {
			return errors.New("document_workshop_task_changed")
		}
	}
	return nil
}

func (d *documentWorkshopPolicy) CallMetadata(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) (map[string]json.RawMessage, error) {
	if err := d.Check(ctx, job, cap); err != nil {
		return nil, err
	}
	inputs := make([]map[string]any, 0, len(d.materialized))
	for _, artifact := range d.materialized {
		inputs = append(inputs, map[string]any{"artifact_id": artifact.AssetID, "path": artifact.InputPath, "mime_type": artifact.MIME, "sha256": artifact.SHA256})
	}
	inputRoot := filepath.Dir(d.materialized[0].InputPath)
	metadata := map[string]any{"inputs": inputs, "input_root": inputRoot, "output_dir": d.materialized[0].OutputDir, "output_root": d.materialized[0].OutputDir}
	// Image OCR is a fixed initial LPAC process. Do this before launching the
	// no-capability Python plugin, which is intentionally unable to create its
	// own child process. The observation is hash-bound to this materialized
	// input and runtime manifest; it never carries a host path.
	observation, err := d.p.documentWorkshopOCR(ctx, d)
	if err != nil {
		return nil, err
	}
	if observation != nil {
		metadata["ocr_observation"] = observation
	}
	meta, _ := json.Marshal(metadata)
	return map[string]json.RawMessage{"wechat/document": meta}, nil
}

func (d *documentWorkshopPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	if err := d.Check(ctx, job, cap); err != nil {
		return pluginruntime.VerifiedResult{}, err
	}
	var out struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Summary  string `json:"summary"`
		Evidence any    `json:"evidence"`
		Outputs  []struct {
			Path        string `json:"path"`
			Role        string `json:"role"`
			MIMEType    string `json:"mime_type"`
			DisplayName string `json:"display_name"`
		} `json:"outputs"`
	}
	if len(raw) > 512<<10 {
		return pluginruntime.VerifiedResult{}, errors.New("document_workshop_result_too_large")
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return pluginruntime.VerifiedResult{}, errors.New("document_workshop_result_not_json")
	}
	if !out.OK {
		if safeDocumentWorkshopCode(out.Error) {
			return pluginruntime.VerifiedResult{}, errors.New("document_workshop_plugin_" + out.Error)
		}
		return pluginruntime.VerifiedResult{}, errors.New("document_workshop_result_not_ok")
	}
	if strings.TrimSpace(out.Summary) == "" {
		return pluginruntime.VerifiedResult{}, errors.New("document_workshop_result_summary_missing")
	}
	// Inspection and verification are deliberately read-only capabilities. A
	// plugin may create task-local transient renders for evidence, but it must
	// not smuggle an output into the publish pipeline through either call.
	if (d.capability == "document.inspect" || d.capability == "document.verify") && len(out.Outputs) != 0 {
		return pluginruntime.VerifiedResult{}, errors.New("document_workshop_read_only_output")
	}
	if (d.capability == "document.reconstruct" || d.capability == "document.export_pdf") && len(out.Outputs) != 1 {
		return pluginruntime.VerifiedResult{}, errors.New("document_workshop_expected_one_output")
	}
	result := map[string]any{"ok": true, "summary": out.Summary, "evidence": sanitizeDocumentEvidence(out.Evidence), "outputs": []map[string]any{}}
	assets := []string{}
	for _, output := range out.Outputs {
		path := filepath.Clean(output.Path)
		root := filepath.Clean(d.materialized[0].OutputDir)
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Base(rel) != rel {
			return pluginruntime.VerifiedResult{}, errors.New("document_workshop_output_path_invalid")
		}
		if err = documentWorkshopOutputType(d.capability, output, path); err != nil {
			return pluginruntime.VerifiedResult{}, err
		}
		// A runtime must return an actual path inside the one host-minted output
		// root; it may never make a sibling input or arbitrary host file appear
		// as a finished document. PublishArtifactForTask repeats regular-file and
		// symlink validation before staging.
		staged, err := d.p.PublishArtifactForTask(ctx, ArtifactPublishRequest{Path: rel, Title: output.DisplayName, TaskID: d.materialized[0].TaskID})
		if err != nil {
			return pluginruntime.VerifiedResult{}, err
		}
		var published struct {
			Media []struct {
				ID      string `json:"id"`
				AssetID string `json:"asset_id"`
			} `json:"media"`
		}
		if json.Unmarshal([]byte(staged), &published) != nil {
			return pluginruntime.VerifiedResult{}, errors.New("document_workshop_publish_invalid")
		}
		if len(published.Media) != 1 || published.Media[0].ID == "" || published.Media[0].AssetID == "" {
			return pluginruntime.VerifiedResult{}, errors.New("document_workshop_output_asset_unavailable")
		}
		media := published.Media[0]
		// asset_id is durable input for the next document_* operation; the
		// separate media id is only for this reply's eventual built-in WeChat
		// delivery. Never make a later export depend on a one-turn upload ID.
		assets = append(assets, media.AssetID)
		result["outputs"] = append(result["outputs"].([]map[string]any), map[string]any{"role": output.Role, "mime_type": output.MIMEType, "display_name": output.DisplayName, "asset_id": media.AssetID, "delivery_media_id": media.ID, "staged": true})
	}
	clean, _ := json.Marshal(result)
	return pluginruntime.VerifiedResult{Assets: assets, Result: clean}, nil
}

func safeDocumentWorkshopCode(value string) bool { return pluginruntime.SafeDocumentResultCode(value) }

// sanitizeDocumentEvidence preserves user-meaningful OCR/layout findings but
// never returns host mount locations, generated scratch render paths, runtime
// installation roots or executable paths to the model. These values are useful
// only to the trusted host and would otherwise invite a later tool call to
// treat a transient pathname as an authorized artifact.
func sanitizeDocumentEvidence(value any) any {
	const maxDepth = 16
	blocked := map[string]bool{
		"path": true, "input_path": true, "input_root": true, "output_dir": true,
		"output_root": true, "output_pdf": true, "runtime_root": true,
		"pdf": true, "text_path": true, "raster_pages": true,
		"render_pages": true, "executable_paths": true,
	}
	var walk func(any, int) any
	walk = func(current any, depth int) any {
		if depth > maxDepth {
			return "[nested evidence omitted]"
		}
		switch data := current.(type) {
		case map[string]any:
			clean := make(map[string]any, len(data))
			for key, item := range data {
				if blocked[strings.ToLower(key)] {
					continue
				}
				clean[key] = walk(item, depth+1)
			}
			return clean
		case []any:
			clean := make([]any, len(data))
			for index, item := range data {
				clean[index] = walk(item, depth+1)
			}
			return clean
		default:
			return current
		}
	}
	// JSON round-tripping turns structs and typed maps from a plugin response
	// into a bounded, ordinary data tree before filtering.
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 384<<10 {
		return map[string]any{"status": "document_evidence_unavailable"}
	}
	var tree any
	if json.Unmarshal(raw, &tree) != nil {
		return map[string]any{"status": "document_evidence_unavailable"}
	}
	return walk(tree, 0)
}

// documentWorkshopOutputType rejects both misleading declared MIME data and a
// renamed arbitrary file before it reaches the artifact/WeChat pipeline. The
// native plugin has exactly two publishing capabilities, each with one fixed
// resulting type.
func documentWorkshopOutputType(capability string, output struct {
	Path        string `json:"path"`
	Role        string `json:"role"`
	MIMEType    string `json:"mime_type"`
	DisplayName string `json:"display_name"`
}, path string) error {
	var extension, role, mimeType string
	switch capability {
	case "document.reconstruct":
		extension, role, mimeType = ".docx", "editable_source", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "document.export_pdf":
		extension, role, mimeType = ".pdf", "delivery_pdf", "application/pdf"
	default:
		return errors.New("document_workshop_output_not_allowed")
	}
	if output.Role != role || !strings.EqualFold(output.MIMEType, mimeType) || !strings.EqualFold(filepath.Ext(path), extension) || !safeMediaFilename(output.DisplayName) {
		return errors.New("document_workshop_output_type_invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("document_workshop_output_unavailable")
	}
	defer file.Close()
	header := make([]byte, 4)
	n, err := file.Read(header)
	if err != nil || n != len(header) {
		return errors.New("document_workshop_output_unavailable")
	}
	if extension == ".pdf" && string(header) != "%PDF" {
		return errors.New("document_workshop_output_type_invalid")
	}
	// DOCX is a ZIP container. Deeper OOXML checks remain in the plugin
	// verifier, while this protects the artifact boundary from obvious spoofing.
	if extension == ".docx" && string(header[:2]) != "PK" {
		return errors.New("document_workshop_output_type_invalid")
	}
	return nil
}

func documentWorkshopJSON(v any) []byte { b, _ := json.Marshal(v); return b }

var _ pluginruntime.CallPolicy = (*documentWorkshopPolicy)(nil)
var _ pluginruntime.CallMetadataPolicy = (*documentWorkshopPolicy)(nil)
