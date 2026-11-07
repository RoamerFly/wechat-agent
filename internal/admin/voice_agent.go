package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/workspacefs"
)

type voiceFile = mediaspec.VoiceFile
type voiceEntry = mediaspec.VoiceEntry

func readVoiceEntries(dir string) ([]voiceEntry, error) {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil, nil
	}
	r, err := workspacefs.EnsurePath(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := workspacefs.OpenRegular(r, "catalog.json")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var catalog struct {
		Items []voiceEntry `json:"items"`
	}
	if json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&catalog) != nil {
		return nil, errors.New("voice_catalog_invalid")
	}
	return catalog.Items, nil
}

func (p *permittedTools) accessibleVoices() ([]voiceEntry, error) {
	public, err := readVoiceEntries(filepath.Join(p.executionContext().Public, "models", "voice-library"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range public {
		public[i].Public = true
		seen[public[i].ID] = true
	}
	if p.privileged() {
		private, err := readVoiceEntries(p.manager.privateVoiceLibrary())
		if err != nil {
			return nil, err
		}
		for _, v := range private {
			if !seen[v.ID] && (v.Owner == "" || v.Owner == p.trace.Sender) {
				public = append(public, v)
			}
		}
	}
	return public, nil
}

func voiceSummary(v voiceEntry) map[string]any {
	return map[string]any{"id": v.ID, "name": v.Name, "role": v.Role, "directory": v.Directory, "engine": v.Engine, "status": v.Status, "public": v.Public, "weight_files": len(v.Weights), "reference_files": len(v.References), "preview_files": len(v.Previews)}
}

func privateVoiceRelative(f voiceFile) (string, error) {
	if f.Relative == "" {
		return filepath.Join("files", f.File), nil
	}
	name := filepath.FromSlash(f.Relative)
	if filepath.IsAbs(name) || name == "." || name == ".." || strings.ContainsAny(name, ":\x00") || filepath.Clean(name) != name || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", errors.New("private_voice_relative_path_invalid")
	}
	return name, nil
}
func (p *permittedTools) voiceTools() []mcptools.Tool {
	return []mcptools.Tool{
		agentTool("voice_library_list", "Discover registered character voices by query/engine. Ordinary members see public voices; trusted owner also sees this account's private GPT-SoVITS/Bailian/RVC archive. No disk scan, synthesis or default changes. Paginated, default limit20; follow next_offset. Input {query?,engine?,offset?,limit?}.", `{"type":"object","properties":{"query":{"type":"string"},"engine":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"additionalProperties":false}`),
		agentTool("voice_library_read", "Read exact discovered voice_id: weight versions, references, current status and matching synthesis route. GPT-SoVITS speech: speech_synthesize(voice_id,text) uses registered project runtime; project_read exposes CLI for custom operation. RVC singing uses returned music_rvc_start profile. No credentials/other accounts; never substitute default voice. Input {voice_id:string}.", `{"type":"object","properties":{"voice_id":{"type":"string"}},"required":["voice_id"],"additionalProperties":false}`),
		agentTool("voice_library_prepare", "Prepare selected voice files with SHA256 checks for file/Shell access. Defaults recommended/latest weight per kind plus references (or previews); file_ids chooses exact hash.ext files from voice_library_read, including older epochs. Personal voices owner only: return existing canonical account voice-library files without duplicate copies; legacy control archives are copied into global. Use server_* global=true. Public models are copied into current conversation. Never mounts credentials, executes weights or sends. Input {voice_id:string,file_ids?:string[]}.", `{"type":"object","properties":{"voice_id":{"type":"string"},"file_ids":{"type":"array","items":{"type":"string"},"minItems":1}},"required":["voice_id"],"additionalProperties":false}`),
	}
}

func (p *permittedTools) callVoiceLibrary(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var in struct {
		ID            string `json:"voice_id"`
		Query, Engine string
		Offset, Limit int
		Files         []string `json:"file_ids"`
	}
	if json.Unmarshal(raw, &in) != nil || in.Offset < 0 || in.Limit < 0 {
		return "", toolInputError("invalid_voice_query")
	}
	voices, err := p.accessibleVoices()
	if err != nil {
		return "", err
	}
	encode := func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }
	if name == "voice_library_list" {
		rows := []map[string]any{}
		for _, v := range voices {
			if (in.Engine == "" || in.Engine == v.Engine) && strings.Contains(strings.ToLower(v.Name+" "+v.Engine), strings.ToLower(in.Query)) {
				rows = append(rows, voiceSummary(v))
			}
		}
		if in.Offset > len(rows) {
			return "", toolInputError("voice_offset_out_of_range")
		}
		if in.Limit == 0 {
			in.Limit = 20
		}
		end := in.Offset + min(in.Limit, len(rows)-in.Offset)
		return encode(map[string]any{"items": rows[in.Offset:end], "total": len(rows), "next_offset": end, "has_more": end < len(rows)})
	}
	for _, v := range voices {
		if v.ID != in.ID {
			continue
		}
		if !validID(v.ID) {
			return "", toolInputError("invalid_registered_voice_id")
		}
		files := append(append(append([]voiceFile{}, v.Weights...), v.References...), v.Previews...)
		for i := range files {
			if files[i].File == "" {
				files[i].File = files[i].Hash + filepath.Ext(files[i].Filename)
			}
		}
		if name == "voice_library_read" {
			row := voiceSummary(v)
			row["notes"], row["files"], row["recommended_weights"], row["synthesis_instruction"] = v.Notes, files, v.Recommended, v.Instruction
			row["engine_compatibility"] = map[string]any{"direct_rvc_cover": strings.Contains(strings.ToLower(v.Engine), "rvc") && v.ProfileID != "", "instruction": "GPT-SoVITS权重用于对应TTS引擎，不是RVC歌声转换权重；能读取不等于可用作原曲换声。指定声线未兼容时不要提交默认声音冒充，也不要把格式差异说成主人权限不足。"}
			row["usage"] = "voice_library_prepare取得可读模型和参考，然后project_list/project_read定位相应引擎。GPT-SoVITS、RVC和百炼音色不互换；权重齐全不等于运行环境和听感已经验证。"
			if strings.EqualFold(v.Engine, "GPT-SoVITS") {
				row["project_id"] = "gpt-sovits"
				row["speech_synthesize"] = map[string]string{"voice_id": v.ID, "text": "填写本次需要合成的原文"}
				row["usage"] = "先project_read({id:gpt-sovits})读取登记的GPT-SoVITS项目、真实解释器和CLI。voice_library_prepare取得明确选定的GPT/SoVITS配对权重、参考音频及其转录；优先speech_synthesize(voice_id,text)。自定义命令在Windows使用server_windows_process(global=true,gpu=true)，Linux使用server_process(global=true,gpu=true)，工作目录选账号受管工作区；不需要访问源码仓库根目录。不因在群里发起而拒绝真实管理员，也不能只查看旧目录后宣称本机未安装。生成后核验实际音频再发送。"
			}
			if v.ProfileID != "" {
				row["music_rvc_start"] = map[string]string{"voice_id": v.ID, "voice_profile_id": v.ProfileID}
				row["spoken_text"] = map[string]string{"tool": "speech_synthesize", "rvc_target_id": v.ID, "usage": "朗读文字时直接传 text 和 rvc_target_id。voice_id 仅选择底稿 TTS；仅有本地语音时需选一个可用 GPT-SoVITS 底稿声线。底稿不投递。"}
			}
			return encode(row)
		}
		if !v.Public && (!p.privileged() || !p.manager.agent.config().CrossConversation) {
			return "", toolInputError("private_voice_prepare_requires_owner_account_global_access")
		}
		base := p.executionContext().Conversation
		if !v.Public {
			base = filepath.Join(p.executionContext().Private, "global")
		}
		dest := filepath.Join(base, "voice-library", v.ID)
		// A migrated private library is already inside the owner's account
		// mount. Verify and return its canonical files instead of duplicating
		// hundreds of MB for every prepare call. Legacy control archives still
		// need an explicit verified copy into the global workspace.
		canonical := !v.Public && p.manager.privateVoiceLibrary() == filepath.Join(p.executionContext().Private, "voice-library")
		if canonical {
			dest = p.manager.privateVoiceLibrary()
		}
		root, e := workspacefs.EnsurePath(dest)
		if e != nil {
			return "", e
		}
		defer root.Close()
		wanted := map[string]bool{}
		for _, id := range in.Files {
			wanted[id] = true
		}
		if len(in.Files) == 0 {
			for _, id := range v.Recommended {
				wanted[id] = true
			}
			if len(v.Recommended) == 0 {
				for _, f := range v.Weights {
					id := f.File
					if id == "" {
						id = f.Hash + filepath.Ext(f.Filename)
					}
					wanted[id] = true
				}
			}
			refs := v.References
			if len(refs) == 0 {
				refs = v.Previews
			}
			for _, f := range refs {
				id := f.File
				if id == "" {
					id = f.Hash + filepath.Ext(f.Filename)
				}
				wanted[id] = true
			}
		}
		selected := []voiceFile{}
		for _, f := range files {
			id := f.File
			if id == "" {
				id = f.Hash + filepath.Ext(f.Filename)
			}
			if !wanted[id] {
				continue
			}
			delete(wanted, id)
			f.File = id
			selected = append(selected, f)
		}
		if len(wanted) > 0 {
			return "", toolInputError("voice_file_not_registered")
		}
		prepared := []map[string]any{}
		for _, f := range selected {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !voiceObjectName.MatchString(f.File) || len(f.Hash) != 64 || !strings.HasPrefix(f.File, f.Hash+".") || f.Bytes <= 0 {
				return "", toolInputError("invalid_voice_file_metadata")
			}
			sourceDir := p.manager.privateVoiceLibrary()
			relative, e := privateVoiceRelative(f)
			if e != nil {
				return "", e
			}
			if v.Public {
				sourceDir = filepath.Join(p.executionContext().Public, "models", "voice-library")
				relative, e = publicVoiceRelative(sourceDir, f)
				if e != nil {
					return "", toolInputError("public_voice_path_outside_models")
				}
			}
			srcRoot, e := workspacefs.EnsurePath(sourceDir)
			if e != nil {
				return "", e
			}
			src, e := workspacefs.OpenRegular(srcRoot, relative)
			target := f.File
			if canonical {
				target = relative
			}
			if e == nil {
				e = publishVerifiedStream(root, target, &uploadContextReader{ctx: ctx, reader: src}, f.Bytes, f.Hash)
				src.Close()
			}
			srcRoot.Close()
			if e != nil {
				return "", e
			}
			prepared = append(prepared, map[string]any{"file_id": f.File, "filename": f.Filename, "path": filepath.Join(dest, target), "sha256": f.Hash, "bytes": f.Bytes, "kind": f.Kind})
		}
		return encode(map[string]any{"voice_id": v.ID, "files": prepared, "global": !v.Public, "directory": dest, "engine": v.Engine, "status": v.Status, "synthesized": false, "wechat_sends": 0, "instruction": "可用server_read/list及隔离Shell读取本次准备的真实文件；私有条目用global=true。只准备文件，未运行模型，不修改默认音色。"})
	}
	return "", toolInputError("voice_not_found_or_not_authorized")
}
