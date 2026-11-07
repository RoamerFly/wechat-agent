package station

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wechat-bot/internal/agenthost"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mediaspec"
)

// Uses an explicitly selected local model with synthetic audio. It never
// calls a provider, reads chats, or creates an application connection.
func TestLiveLocalStationASRThroughWindowsIsolation(t *testing.T) {
	resources := os.Getenv("AHS_LOCAL_ASR_RESOURCES")
	if runtime.GOOS != "windows" || resources == "" {
		t.Skip("opt in with AHS_LOCAL_ASR_RESOURCES and AHS_LOCAL_PROJECT_TOOL_RUNTIME_ROOT")
	}
	options := localProjectToolWindowsOptionsFor(t, localasr.PluginID)
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "home"))
	f.handler.home.projectTools.options = options
	f.handler.home.windows = &agenthost.WindowsConfig{
		Pythonw: options.Pythonw, Helper: filepath.Join(filepath.Dir(options.WindowsHelper), "windows_agent_host.pyw"),
		Spool: filepath.Join(base, "spool"),
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = filepath.WalkDir(filepath.Join(f.dir, "station-asr-spool"), func(path string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && (entry.Name() == "stderr.log" || entry.Name() == "result.json") {
					if raw, err := os.ReadFile(path); err == nil {
						t.Logf("synthetic ASR %s: %s", entry.Name(), raw)
					}
				}
				return nil
			})
		}
	})
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatal("initialize", code, out)
	}
	registration, err := json.Marshal(localStationASRRegistration{SchemaVersion: 1, Enabled: true, Workspace: resources, ProfileID: "sensevoice-small-int8"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.dir, localStationASRRegistrationFile), registration, 0600); err != nil {
		t.Fatal(err)
	}
	if code, out := f.request("PUT", "/api/local/project-tools/"+localasr.PluginID, map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatal("install", code, out)
	}
	installed := localProjectToolByID(t, f, localasr.PluginID)
	if code, out := f.request("PUT", "/api/local/project-tools/"+localasr.PluginID, map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatal("enable", code, out)
	}
	wav := make([]byte, 44+16000*2)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(wav)-44))
	text, err := f.handler.home.transcribeLocalStationAudio(t.Context(), wav, func() error { return nil })
	var failure *mediaspec.Failure
	if text != "" || !errors.As(err, &failure) || failure.Code != "ASR_RESPONSE_HAVE_NO_WORDS" {
		t.Fatalf("silence must be a verified no-speech result: text=%q error=%v", text, err)
	}
	if input := os.Getenv("AHS_LOCAL_ASR_SYNTHETIC_WAV"); input != "" {
		wav, err = os.ReadFile(input)
		if err != nil {
			t.Fatal(err)
		}
		text, err = f.handler.home.transcribeLocalStationAudio(t.Context(), wav, func() error { return nil })
		if err != nil || !strings.Contains(strings.ToLower(text), "local speech recognition test") {
			t.Fatalf("synthetic English speech was not recognized: text=%q error=%v", text, err)
		}
	}
}
