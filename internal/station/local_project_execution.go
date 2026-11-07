package station

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
	"wechat-bot/internal/workspacefs"
	catalog "wechat-bot/plugins"
)

type localProjectOperation struct {
	pkg, capability, inputExt string
	inputLimit                int64
}

func projectOperation(name string) localProjectOperation {
	if name == localAudioToolName {
		return localProjectOperation{pkg: localAudioPackage, capability: localAudioCapability, inputExt: ".wav", inputLimit: stationAttachmentLimit}
	}
	capability := documentToolCapability(name)
	if capability == "" {
		return localProjectOperation{}
	}
	operation := localProjectOperation{pkg: pluginruntime.DocumentWorkshopID, capability: capability}
	if capability != localDocumentCapability {
		operation.inputExt = ".docx"
		operation.inputLimit = 128 << 20
	}
	return operation
}

func (t *localToolSet) projectHostForDiscovery() *localProjectToolHost {
	if t.projectTools == nil || t.run == nil || !t.run.Tracked || t.access == nil || !t.projectTools.runtimeReady() {
		return nil
	}
	grant, err := t.access.resolve()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(t.projectTools.home.dataDir, "project-tools", grant.FishID)); err != nil {
		return nil
	}
	host, _ := t.projectTools.forFish(grant.FishID)
	return host
}

func installedLocalProject(ctx context.Context, host *localProjectToolHost, operation localProjectOperation) (pluginhost.InstalledPackage, error) {
	item, err := host.store.Installed(ctx, operation.pkg)
	contract, contractErr := catalog.ContractFor(operation.pkg)
	if err != nil || contractErr != nil || !item.Enabled || item.Removed || item.Digest != contract.ManifestSHA256 && !compatibleDocumentDigest(operation, item.Digest) {
		return item, errors.New("项目工具未启用或安装版本待核对")
	}
	return item, nil
}

func compatibleDocumentDigest(operation localProjectOperation, digest string) bool {
	if operation.pkg != pluginruntime.DocumentWorkshopID {
		return false
	}
	switch operation.capability {
	case localDocumentCapability:
		return digest == localDocumentLegacyDigest || digest == localDocumentV012Digest || digest == localDocumentV013Digest || digest == localDocumentV014Digest || digest == localDocumentV015Digest
	case localDocumentInspect, localDocumentExport:
		return digest == localDocumentV012Digest || digest == localDocumentV013Digest || digest == localDocumentV014Digest || digest == localDocumentV015Digest
	case localDocumentRevise:
		return digest == localDocumentV013Digest || digest == localDocumentV014Digest || digest == localDocumentV015Digest
	}
	return false
}

func (t *localToolSet) invokeLocalProjectTool(ctx context.Context, tool string, arguments json.RawMessage, inputID string) (string, error) {
	operation := projectOperation(tool)
	capability := operation.capability
	if capability == "" || t.projectTools == nil || t.run == nil || !t.run.Tracked {
		return "", errors.New("当前本机对话没有项目工具执行授权")
	}
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", err
	}
	source, ok := t.store.SourceForInput(companion.Input{Channel: "local", Account: grant.FishID, Thread: grant.OwnerID, Sender: grant.OwnerID, TraceID: t.traceID})
	if !ok {
		return "", errors.New("本轮来源已变化")
	}
	versions := map[string]int{source.EventID: source.Revision}
	var input *localProjectArtifactInput
	if inputID != "" {
		input, err = t.access.home.artifacts.bindProjectArtifact(ctx, inputID, t.access, grant)
		if err != nil {
			return "", err
		}
		artifact := input.artifact
		if !strings.EqualFold(filepath.Ext(artifact.Name), operation.inputExt) || artifact.Bytes > operation.inputLimit {
			return "", mcptools.ArgumentError("$.artifact_id", fmt.Sprintf("需要不超过 %d MiB 的已登记 %s 成品", operation.inputLimit>>20, operation.inputExt))
		}
		if artifact.Source != nil {
			versions[artifact.Source.EventID] = artifact.Source.Revision
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "", err
		}
		delete(args, "artifact_id")
		args["artifact"], _ = json.Marshal(localArtifactLink{ID: artifact.ID, SHA256: artifact.SHA256})
		arguments, _ = json.Marshal(args)
	}
	op := companion.LocalToolCheckpoint{Tool: tool, Grant: localCheckpointKey(grant), Target: inputID,
		Key: localCheckpointKey(tool, source.EventID, source.Revision, json.RawMessage(arguments))}
	for _, previous := range t.run.Operations {
		if previous.Key == op.Key {
			return localProcessJSON(t.checkpointView(previous, grant))
		}
	}
	if !t.projectTools.runtimeReady() {
		return "", errors.New("Windows 项目工具运行器缺失")
	}
	if capability == localDocumentExport && !t.projectTools.pdfRuntimeReady() {
		return "", errors.New("当前发行包未配齐 PDF 运行时，请从项目工具查看状态")
	}
	host, err := t.projectTools.forFish(grant.FishID)
	if err != nil {
		return "", err
	}
	installed, err := installedLocalProject(ctx, host, operation)
	if err != nil {
		return "", err
	}
	cfg, err := host.store.Configuration(ctx, operation.pkg)
	if err != nil {
		return "", errors.New("项目工具配置不可读")
	}
	invocation := pluginhost.Invocation{Account: grant.FishID, Thread: grant.OwnerID, Requester: grant.OwnerID,
		Purpose: "local project " + capability, RequestKey: op.Key, Authorization: op.Grant, SourceVersions: versions}
	policy := &localProjectPolicy{tool: t, host: host, grant: grant, source: source, installed: installed, configRevision: cfg.Revision,
		invocation: invocation, capability: capability, input: input}
	controller := host.controller
	if input != nil && !t.inspectOnly {
		scoped, closeTask, err := policy.inputRuntime(ctx)
		if err != nil {
			return "", err
		}
		defer t.projectTools.markActive(operation.pkg)()
		defer closeTask()
		controller = scoped
	}
	return t.dispatchCheckpointed(ctx, op, grant, func() (string, string, error) {
		if err := controller.EnsureStarted(ctx, operation.pkg, installed.Revision); err != nil {
			return "", "", errors.New("项目工具未能启动，请在项目工具页核对运行状态")
		}
		result, err := controller.Invoke(ctx, pluginruntime.CallRequest{Plugin: operation.pkg,
			Capability: capability, Context: invocation, Arguments: arguments}, policy)
		if err != nil {
			return "", "", err
		}
		if input != nil {
			if err := input.check(ctx); err != nil {
				return "", "", err
			}
		}
		view := t.localProjectResult(ctx, tool, result.Job)
		data, err := localProcessJSON(view)
		return data, "recorded", err
	})
}

func (t *localToolSet) localProjectResult(ctx context.Context, tool string, job pluginhost.Job) map[string]any {
	view := map[string]any{"tool": tool, "task_id": job.ID, "state": job.State}
	if job.State == "failed" {
		var failure struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(job.Result, &failure) == nil && (pluginruntime.SafeDocumentResultCode(failure.Reason) || tool == localAudioToolName && safeLocalAudioCode(failure.Reason)) {
			view["reason"] = failure.Reason
		}
		return view
	}
	if job.State != "completed" || ctx.Err() != nil || t.run.CheckSource(ctx) != nil {
		return view
	}
	if tool == localDocumentInspectTool && len(job.Artifacts) == 0 {
		view["status"], view["evidence"] = "inspected", json.RawMessage(job.Result)
		return view
	}
	if len(job.Artifacts) != 1 {
		return view
	}
	store := t.access.home.artifacts
	artifact, err := store.read(job.Artifacts[0])
	if err == nil && artifact.Request == job.ID {
		file, err := store.open(ctx, artifact, func() bool { _, allowed := store.access(artifact); return allowed })
		if err == nil {
			file.Close()
			view = artifact.view()
			view["tool"], view["task_id"] = tool, job.ID
			if tool == localDocumentExportTool || tool == localAudioToolName {
				view["verification"] = json.RawMessage(job.Result)
			}
			return view
		}
	}
	view["state"] = "artifact_unavailable"
	return view
}

func (t *localToolSet) projectCheckpointView(op companion.LocalToolCheckpoint, grant localWorkspaceGrant) map[string]any {
	view := map[string]any{"tool": op.Tool, "state": "needs_review", "recovered": true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if t.projectTools == nil || t.run == nil || t.run.CheckSource(ctx) != nil {
		return view
	}
	host, err := t.projectTools.forFish(grant.FishID)
	if err != nil {
		return view
	}
	operation := projectOperation(op.Tool)
	job, err := host.store.JobByRequest(ctx, operation.pkg, grant.OwnerID, op.Key)
	if err != nil || job.Context.Authorization != op.Grant || job.Context.Requester != grant.OwnerID || job.Context.Account != grant.FishID {
		return view
	}
	if job.Tool != operation.capability {
		return view
	}
	if op.Target != "" {
		var args struct {
			Artifact localArtifactLink `json:"artifact"`
		}
		artifact, err := t.access.home.artifacts.bindProjectArtifact(ctx, op.Target, t.access, grant)
		if err != nil || json.Unmarshal(job.Arguments, &args) != nil ||
			args.Artifact.ID != op.Target || args.Artifact.SHA256 != artifact.artifact.SHA256 {
			view["state"] = "artifact_unavailable"
			return view
		}
	}
	view = t.localProjectResult(ctx, op.Tool, job)
	view["recovered"] = true
	return view
}

type localProjectPolicy struct {
	tool           *localToolSet
	host           *localProjectToolHost
	grant          localWorkspaceGrant
	source         *companion.SourceRef
	installed      pluginhost.InstalledPackage
	configRevision int
	invocation     pluginhost.Invocation
	capability     string
	input          *localProjectArtifactInput
	inputPath      string
	outputPath     string
}

// Reading an existing private artifact requires a task-scoped LPAC mount.
// The long-lived creation worker never sees input copies from other requests.
func (p *localProjectPolicy) inputRuntime(ctx context.Context) (*pluginruntime.Controller, func(), error) {
	parent := filepath.Join(p.tool.projectTools.home.dataDir, "project-tool-tasks")
	options := p.tool.projectTools.options
	if p.capability != localDocumentExport {
		options.DocumentRuntimeRoot = ""
	}
	controller, input, output, cleanup, err := openScopedProjectRuntime(ctx, parent, p.host.store, options, p.installed.Package.ID, false, p.input.materialize)
	p.inputPath, p.outputPath = input, output
	return controller, cleanup, err
}

func openScopedProjectRuntime(ctx context.Context, parent string, store *pluginhost.Store, options pluginruntime.Options, packageID string, discardOutput bool,
	materialize func(context.Context, string) (localProjectArtifactMaterialized, error)) (*pluginruntime.Controller, string, string, func(), error) {
	root, err := workspacefs.EnsurePath(parent)
	if err != nil {
		return nil, "", "", nil, err
	}
	if err := root.Close(); err != nil {
		return nil, "", "", nil, err
	}
	dir, err := os.MkdirTemp(parent, "task-")
	if err != nil {
		return nil, "", "", nil, err
	}
	inputDir := filepath.Join(dir, "input")
	outputDir := filepath.Join(dir, "output")
	var inputPath string
	var controller *pluginruntime.Controller
	cleanup := func() {
		if controller != nil {
			_ = controller.Close()
		}
		if inputPath != "" {
			_ = os.Remove(inputPath)
		}
		if discardOutput {
			// Read-only operations produce no recoverable file. The launcher
			// may still create temporary children under its writable mount.
			if root, err := workspacefs.OpenPath(parent); err == nil {
				_ = root.RemoveAll(filepath.Base(dir))
				_ = root.Close()
			}
			return
		}
		_ = os.Remove(inputDir)
		_ = os.Remove(outputDir)
		_ = os.Remove(dir)
	}
	if err := os.Mkdir(inputDir, 0700); err != nil {
		cleanup()
		return nil, "", "", nil, err
	}
	if err := os.Mkdir(outputDir, 0700); err != nil {
		cleanup()
		return nil, "", "", nil, err
	}
	materialized, err := materialize(ctx, inputDir)
	if err != nil {
		cleanup()
		return nil, "", "", nil, err
	}
	inputPath = materialized.InputPath
	executor := pluginruntime.InstalledExecutor(store, options)
	resolve := executor.Resolve
	executor.Resolve = func(ctx context.Context, pkg pluginhost.Package, grant pluginhost.Grant) (pluginruntime.LaunchSpec, error) {
		spec, err := resolve(ctx, pkg, grant)
		if err != nil || pkg.ID != packageID {
			return spec, errors.New("项目工具隔离环境不可用")
		}
		spec.DataDir = outputDir
		spec.ReadOnlyDirs = append(spec.ReadOnlyDirs, inputDir)
		spec.ReadOnlyFiles = append(spec.ReadOnlyFiles, inputPath)
		return spec, nil
	}
	controller, err = pluginruntime.NewController(store, executor)
	if err != nil {
		cleanup()
		return nil, "", "", nil, err
	}
	return controller, inputPath, outputDir, cleanup, nil
}

func (p *localProjectPolicy) Validate(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, raw json.RawMessage) (pluginruntime.VerifiedResult, error) {
	if p.capability == localAudioCapability {
		return p.validateAudio(ctx, job, cap, raw)
	}
	return p.validateDocument(ctx, job, cap, raw)
}

func (p *localProjectPolicy) Check(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.Plugin != p.installed.Package.ID || job.Tool != p.capability || cap.Async ||
		job.PackageDigest != p.installed.Digest || job.Version != p.installed.Package.Version ||
		job.ConfigRevision != strconv.Itoa(p.configRevision) || !reflect.DeepEqual(job.Context, p.invocation) ||
		!p.tool.access.current(p.grant) || p.tool.run.CheckSource(ctx) != nil {
		return errors.New("项目工具任务来源或本机授权已变化")
	}
	if p.input != nil {
		if err := p.input.check(ctx); err != nil {
			return err
		}
	}
	current, err := p.host.store.Installed(ctx, job.Plugin)
	if err != nil || !current.Enabled || current.Removed || current.Revision != p.installed.Revision || current.Digest != p.installed.Digest {
		return errors.New("项目工具版本或授权已变化")
	}
	cfg, err := p.host.store.Configuration(ctx, job.Plugin)
	if err != nil || cfg.Revision != p.configRevision {
		return errors.New("项目工具配置已变化")
	}
	if job.ID != "" {
		currentJob, err := p.host.store.Job(ctx, job.ID)
		if err != nil || currentJob.Revision != job.Revision || currentJob.CancelRequested {
			return errors.New("项目工具任务已取消或状态变化")
		}
	}
	return nil
}

func (p *localProjectPolicy) outputDirectory(ctx context.Context, job pluginhost.Job) (string, error) {
	if p.outputPath != "" {
		return p.outputPath, nil
	}
	if !artifactID("job-"+job.ID, "job-") {
		return "", errors.New("项目工具任务编号无效")
	}
	private, err := p.host.store.PrivateDirectory(ctx, job.Plugin)
	if err != nil {
		return "", err
	}
	return filepath.Join(private, job.ID), nil
}

func (p *localProjectPolicy) CallMetadata(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability) (map[string]json.RawMessage, error) {
	if err := p.Check(ctx, job, cap); err != nil {
		return nil, err
	}
	dir, err := p.outputDirectory(ctx, job)
	if err != nil {
		return nil, err
	}
	// The process creates the child directory inside its LPAC-mounted data
	// root. Host-created children are not guaranteed to inherit the live SID
	// grant. The registrar opens the returned file without following links.
	mount := windowsProjectToolPath(dir)
	if p.capability == localAudioCapability {
		input := p.input.artifact
		data, err := json.Marshal(map[string]any{
			"input":      map[string]string{"artifact_id": input.ID, "sha256": input.SHA256, "path": windowsProjectToolPath(p.inputPath), "mime_type": "audio/wav"},
			"input_root": windowsProjectToolPath(filepath.Dir(p.inputPath)), "output_root": mount, "output_dir": mount,
		})
		return map[string]json.RawMessage{"station/audio": data}, err
	}
	metadata := map[string]any{"inputs": []any{}, "output_dir": mount, "output_root": mount}
	if p.input != nil {
		artifact := p.input.artifact
		metadata["inputs"] = []map[string]string{{"artifact_id": artifact.ID, "sha256": artifact.SHA256,
			"path": windowsProjectToolPath(p.inputPath), "mime_type": localDOCXMIME}}
		metadata["input_root"] = windowsProjectToolPath(filepath.Dir(p.inputPath))
	}
	data, _ := json.Marshal(metadata)
	return map[string]json.RawMessage{"wechat/document": data}, nil
}

func (p *localProjectPolicy) publishOutput(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, path, name, extension, digest string, validate func(*os.File, int64) error) (localArtifact, error) {
	dir, err := p.outputDirectory(ctx, job)
	if err != nil {
		return localArtifact{}, err
	}
	rel, err := filepath.Rel(windowsProjectToolPath(dir), windowsProjectToolPath(path))
	if err != nil || !filepath.IsAbs(path) || rel != name || !filepath.IsLocal(rel) || !mediaspec.SafeFilename(rel) || rel != filepath.Base(rel) ||
		!strings.EqualFold(filepath.Ext(rel), extension) {
		return localArtifact{}, errors.New("项目工具输出路径或格式不符合任务范围")
	}
	p.tool.projectTools.update.Lock()
	defer p.tool.projectTools.update.Unlock()
	if err := p.Check(ctx, job, cap); err != nil {
		return localArtifact{}, err
	}
	input := localArtifactInput{Expected: digest, Key: job.ID}
	if p.input != nil {
		input.parent = &localArtifactLink{ID: p.input.artifact.ID, SHA256: p.input.artifact.SHA256}
	}
	artifact, err := p.tool.access.home.artifacts.publishFile(ctx, input, p.source, p.tool.access, p.grant, filepath.Join(dir, rel), validate)
	if err != nil {
		return localArtifact{}, err
	}
	if err := p.Check(ctx, job, cap); err != nil {
		return localArtifact{}, err
	}
	// Unknown jobs keep their private files for reconciliation.
	if root, err := workspacefs.OpenPath(dir); err == nil {
		_ = root.Remove(rel)
		root.Close()
		_ = os.Remove(dir)
	}
	return artifact, nil
}

// CPython running inside LPAC can hit MAX_PATH even on machines with the
// long-path registry flag. Pass extended paths to the tool, and use ordinary
// host paths for the separately validated artifact read.
func windowsProjectToolPath(path string) string {
	return pluginruntime.WindowsProcessPath(path)
}
