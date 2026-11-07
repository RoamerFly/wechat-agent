package admin

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/mcptools"
)

// A capability adapter owns its discovery and invocation together. The common
// Call entry point still checks live permissions and normalizes arguments
// before dispatch; registering a module never grants it additional authority.
type capabilityAdapter struct {
	id    string
	list  func() []mcptools.Tool
	match func(string) bool
	call  func(context.Context, string, json.RawMessage) (string, error)
}

func namedTools(names ...string) func(string) bool {
	return func(name string) bool {
		for _, n := range names {
			if name == n {
				return true
			}
		}
		return false
	}
}

func (p *permittedTools) capabilities() []capabilityAdapter {
	return []capabilityAdapter{
		{"private-voice-enrollment", p.enrollmentTools, namedTools("voice_enroll"), p.callEnrollment},
		{"installed-services", p.servicePluginTools, namedTools("service_plugins", "service_plugin_call", "service_plugin_task"), p.callServicePlugin},
		{"video-inspection", p.videoTools, namedTools("video_inspect"), p.callVideoInspection},
		{"wechat-communication", p.wechatCommunicationTools, wechatapp.ToolName, p.callAgent},
		{"workspace-agent", p.workspaceAgentTools, workspaceAgentToolName, p.callAgent},
		{"mobile", p.mobileTools, func(n string) bool { return strings.HasPrefix(n, "device_") }, p.callMobile},
		{"plugin-jobs", p.pluginTaskTools, namedTools("plugin_job_status", "plugin_job_cancel"), p.callPluginJob},
		{"bailian-speech", p.speechTools, namedTools("speech_synthesize", "music_generate"), p.callSpeech},
		{"yue", p.yueTools, namedTools("music_rvc_start", "music_sampling_options", "music_local_start", "music_local_status", "music_local_resume", "music_reference_inspect", "music_lyrics_compare", "music_service_status"), p.callYuE},
		{"run-output", p.outputTools, namedTools("tool_output_read"), p.readToolOutput},
		{"surf-interaction", p.surfInteractionTools, namedTools("surf_interaction"), p.callSurfInteraction},
	}
}

// Provider deadlines are adapter policy, not extra action/round quotas in the
// Agent loop. A zero value retains the run deadline and a tool's own deadline.
func (p *permittedTools) ToolTimeout(name string) time.Duration {
	if webServiceCapability(name) != "" {
		if installed, _ := p.installedWebState(); installed {
			return 0
		}
	}
	switch name {
	case "web_search", "web_fetch":
		return 25 * time.Second
	case "web_audio_search":
		return time.Minute
	case "web_media_download":
		return 6 * time.Minute
	}
	return 0
}
