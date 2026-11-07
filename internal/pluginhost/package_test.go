package pluginhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDependencyConstraintContract(t *testing.T) {
	for _, tc := range []struct {
		requirement, version string
		want                 bool
	}{
		{"", "opaque-legacy", true}, {"^1.2.0", "1.9.3", true}, {"^1.2.0", "2.0.0", false},
		{"~1.2.0", "1.2.8", true}, {"~1.2.0", "1.3.0", false}, {">=1.2.0 <2.0.0", "1.2.0", true},
		{">=1.2.0 <2.0.0", "2.0.0", false}, {"1.2", "1.2.0", false}, {"^0.1.1", "0.1.9", true},
		{"^0.1.1", "0.2.0", false}, {"^0.0.1", "0.0.2", false}, {"^0.0.1", "0.0.1", true},
		{"^1.2.3", "+1.2.3", false}, {" ", "1.2.3", false},
	} {
		if got := DependencyCompatible(tc.requirement, tc.version); got != tc.want {
			t.Fatalf("%q against %q = %v", tc.requirement, tc.version, got)
		}
	}
	var legacy Package
	if err := json.Unmarshal([]byte(`{"dependencies":["media-understanding"]}`), &legacy); err != nil || len(legacy.Dependencies) != 1 || legacy.Dependencies[0].ID != "media-understanding" {
		t.Fatal(err, legacy.Dependencies)
	}
	var fixed Package
	if err := json.Unmarshal([]byte(`{"dependencies":[{"id":"media-understanding","version":"^1.2.0"}]}`), &fixed); err != nil || fixed.Dependencies[0].Version != "^1.2.0" {
		t.Fatal(err, fixed.Dependencies)
	}
	if validDependencyConstraint("1.2") || validDependencyConstraint(" ") || !validDependencyConstraint("^1.2.0") {
		t.Fatal("constraint validation")
	}
	for _, raw := range []string{`{"dependencies":[{"id":"media-understanding","version":"^1.2.0","surprise":true}]}`, `{"dependencies":[{"id":"media-understanding","version":"^1.2.0"},"legacy"]}`} {
		var p Package
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			t.Fatal("ambiguous dependency declaration accepted", raw)
		}
	}
}

func TestOfficialContracts(t *testing.T) {
	dirs, err := os.ReadDir("../../plugins")
	if err != nil {
		t.Fatal(err)
	}
	folders := dirs[:0]
	for _, d := range dirs {
		if d.IsDir() {
			folders = append(folders, d)
		}
	}
	dirs = folders
	if len(dirs) != 16 {
		t.Fatalf("expected 16 capability packages, got %d", len(dirs))
	}
	for _, d := range dirs {
		t.Run(d.Name(), func(t *testing.T) {
			p, e := Verify(filepath.Join("../../plugins", d.Name()))
			if e != nil {
				t.Fatal(e)
			}
			if d.Name() == "web-search" || d.Name() == "media-understanding" || d.Name() == "speech" || d.Name() == "music" || d.Name() == "share-reader" || d.Name() == "mobile-control" || d.Name() == "project-development" || d.Name() == "external-tools" || d.Name() == "document-workshop" || d.Name() == "audio-workshop" || d.Name() == "rvc" || d.Name() == "comfyui" || d.Name() == "yue" || d.Name() == "station-speech" || d.Name() == "obs-preview" {
				transport, entrypoint := "mcp-stdio", "runtime/main.py"
				if d.Name() == "rvc" || d.Name() == "comfyui" || d.Name() == "yue" || d.Name() == "station-speech" || d.Name() == "obs-preview" {
					transport, entrypoint = "http-adapter", "runtime/managed-service.json"
				}
				if p.Status != "ready" || p.Runtime.Transport != transport || p.Runtime.Entrypoint != entrypoint {
					t.Fatal("web runtime not connected")
				}
				if d.Name() != "web-search" {
					want := map[string]bool{"media.observe": true, "media.transcribe": true, "media.listen": true}
					if d.Name() == "speech" {
						want = map[string]bool{"speech.synthesize": true, "speech.synthesize_named": true}
					}
					if d.Name() == "music" {
						want = map[string]bool{"music.generate": true, "music.bridge_request": true, "music.service_control": true, "music.prepare": true}
						if len(p.Permissions) != 2 || p.Permissions[0] != "model.invoke" || p.Permissions[1] != "network" {
							t.Fatal("music package must declare separate cloud and local bridge proxies")
						}
					}
					if d.Name() == "share-reader" {
						want = map[string]bool{"share.resolve_public": true, "share.resolve_channels": true}
					}
					if d.Name() == "project-development" {
						want = map[string]bool{"project.workspace_read": true, "project.workspace_write": true, "project.workspace_execute": true}
						if len(p.Permissions) != 3 || p.Permissions[0] != "workspace.read" || p.Permissions[1] != "workspace.write" || p.Permissions[2] != "process.execute" {
							t.Fatal("project package must declare three separate workspace/process proxies")
						}
					}
					if d.Name() == "external-tools" {
						want = map[string]bool{"external.http_request": true, "external.job_request": true, "external.poll_request": true, "external.mcp_request": true}
						if len(p.Permissions) != 1 || p.Permissions[0] != "network" {
							t.Fatal("external HTTP must use the bounded network proxy only")
						}
					}
					if d.Name() == "mobile-control" {
						want = map[string]bool{"device.workflow": true, "device.plan": true, "device.list": true, "device.observe": true, "device.act": true, "device.record": true, "device.read_artifact": true}
						if len(p.Permissions) != 2 || p.Permissions[0] != "model.invoke" || p.Permissions[1] != "device" {
							t.Fatal("device package must declare only separate model/device proxies")
						}
					}
					if d.Name() == "document-workshop" {
						want = map[string]bool{"document.inspect": true, "document.reconstruct": true, "document.revise_title": true, "document.export_pdf": true, "document.verify": true}
						if len(p.Permissions) != 2 || p.Permissions[0] != "artifact.read" || p.Permissions[1] != "artifact.write" {
							t.Fatal("document workshop must use only artifact read/write permissions")
						}
					}
					if d.Name() == "audio-workshop" {
						want = map[string]bool{"audio.prepare": true}
						if len(p.Permissions) != 2 || p.Permissions[0] != "artifact.read" || p.Permissions[1] != "artifact.write" {
							t.Fatal("audio workshop must use only artifact read/write permissions")
						}
					}
					if d.Name() == "rvc" {
						want = map[string]bool{"rvc.convert": true}
						if len(p.Permissions) != 3 || p.Permissions[0] != "artifact.read" || p.Permissions[1] != "artifact.write" || p.Permissions[2] != "gpu" {
							t.Fatal("rvc package must declare only artifact read/write and explicit GPU permissions")
						}
					}
					if d.Name() == "comfyui" {
						want = map[string]bool{"comfyui.workflows": true, "comfyui.run": true, "comfyui.job": true}
						if len(p.Permissions) != 2 || p.Permissions[0] != "artifact.write" || p.Permissions[1] != "gpu" {
							t.Fatal("comfyui package must declare only artifact write and explicit GPU permissions")
						}
					}
					if d.Name() == "yue" {
						want = map[string]bool{"yue.generate": true, "yue.job": true}
						if len(p.Permissions) != 3 || p.Permissions[0] != "artifact.read" || p.Permissions[1] != "artifact.write" || p.Permissions[2] != "gpu" {
							t.Fatal("YuE package must declare only artifact read/write and explicit GPU permissions")
						}
					}
					if d.Name() == "station-speech" {
						want = map[string]bool{"station-speech.voices": true, "station-speech.synthesize": true, "station-speech.job": true}
						if len(p.Permissions) != 2 || p.Permissions[0] != "artifact.write" || p.Permissions[1] != "gpu" {
							t.Fatal("station speech package must declare only artifact write and explicit GPU permissions")
						}
					}
					if d.Name() == "obs-preview" {
						want = map[string]bool{"obs.preview": true}
						if len(p.Permissions) != 1 || p.Permissions[0] != "network" {
							t.Fatal("OBS preview must declare only the bounded local network proxy")
						}
					}
					raw, err := os.ReadFile(filepath.Join("../../plugins", d.Name(), "capabilities.json"))
					if err != nil {
						t.Fatal(err)
					}
					var doc struct {
						Capabilities []Capability `json:"capabilities"`
					}
					if err = json.Unmarshal(raw, &doc); err != nil {
						t.Fatal(err)
					}
					for _, cap := range doc.Capabilities {
						if want[cap.ID] {
							if cap.Implementation != "ready" || (d.Name() != "obs-preview" && !cap.Discoverable) || (d.Name() == "obs-preview" && cap.Discoverable) {
								t.Fatal("implemented capability hidden", cap.ID)
							}
							if d.Name() == "mobile-control" {
								permission := "device"
								if cap.ID == "device.plan" {
									permission = "model.invoke"
								}
								if cap.ID == "device.workflow" {
									if cap.Async || len(cap.Permissions) != 2 || cap.Permissions[0] != "device" || cap.Permissions[1] != "model.invoke" {
										t.Fatal("pure workflow must remain synchronous and require both scoped host authorities")
									}
								} else if cap.Async || len(cap.Permissions) != 1 || cap.Permissions[0] != permission {
									t.Fatal("device capability must remain synchronous and use only its own proxy")
								}
							}
							if d.Name() == "music" {
								permission := "model.invoke"
								if cap.ID != "music.generate" {
									permission = "network"
								}
								if cap.Async || len(cap.Permissions) != 1 || cap.Permissions[0] != permission {
									t.Fatal("music capability must remain synchronous and use its own proxy")
								}
							}
							if d.Name() == "external-tools" && (cap.Async || len(cap.Permissions) != 1 || cap.Permissions[0] != "network") {
								t.Fatal("external HTTP capability must remain synchronous and use its network proxy")
							}
							if d.Name() == "project-development" {
								permission := map[string]string{
									"project.workspace_read":    "workspace.read",
									"project.workspace_write":   "workspace.write",
									"project.workspace_execute": "process.execute",
								}[cap.ID]
								if cap.Async || len(cap.Permissions) != 1 || cap.Permissions[0] != permission {
									t.Fatal("project capability must use only its corresponding host proxy", cap.ID)
								}
							}
							if d.Name() == "speech" {
								wantPermissions := []string{"model.invoke"}
								if cap.ID == "speech.synthesize_named" {
									wantPermissions = []string{"artifact.read", "model.invoke"}
								}
								if cap.Async || len(cap.Permissions) != len(wantPermissions) {
									t.Fatal("speech capability must remain synchronous with exact proxy permissions", cap.ID)
								}
								for i, permission := range wantPermissions {
									if cap.Permissions[i] != permission {
										t.Fatal("speech capability permission changed", cap.ID, cap.Permissions)
									}
								}
							}
							if d.Name() == "media-understanding" && (cap.Async || len(cap.Permissions) != 2 || cap.Permissions[0] != "artifact.read" || cap.Permissions[1] != "model.invoke") {
								t.Fatal("media capability must remain synchronous and use only artifact/model proxies", cap.ID)
							}
							if d.Name() == "rvc" && (!cap.Async || cap.SideEffect != "artifact" || len(cap.Permissions) != 3 || cap.Permissions[0] != "artifact.read" || cap.Permissions[1] != "artifact.write" || cap.Permissions[2] != "gpu") {
								t.Fatal("rvc conversion must remain asynchronous and use only artifact/GPU host authorities", cap.ID)
							}
							if d.Name() == "comfyui" {
								wantPermissions := []string{"gpu"}
								if cap.ID != "comfyui.workflows" {
									wantPermissions = []string{"artifact.write", "gpu"}
								}
								if (cap.ID == "comfyui.run") != cap.Async || len(cap.Permissions) != len(wantPermissions) {
									t.Fatal("comfyui capability must retain its scoped asynchronous contract", cap.ID)
								}
								for i, permission := range wantPermissions {
									if cap.Permissions[i] != permission {
										t.Fatal("comfyui capability permission changed", cap.ID, cap.Permissions)
									}
								}
							}
							if d.Name() == "yue" {
								wantPermissions := []string{"artifact.read", "artifact.write", "gpu"}
								if (cap.ID == "yue.generate") != cap.Async || len(cap.Permissions) != len(wantPermissions) {
									t.Fatal("YuE capability must retain its scoped asynchronous contract", cap.ID)
								}
								for i, permission := range wantPermissions {
									if cap.Permissions[i] != permission {
										t.Fatal("YuE capability permission changed", cap.ID, cap.Permissions)
									}
								}
							}
							if d.Name() == "station-speech" {
								wantPermissions := []string{"artifact.write", "gpu"}
								if cap.Async || len(cap.Permissions) != len(wantPermissions) {
									t.Fatal("station speech capability must wait within the original local chat turn", cap.ID)
								}
								for i, permission := range wantPermissions {
									if cap.Permissions[i] != permission {
										t.Fatal("station speech capability permission changed", cap.ID, cap.Permissions)
									}
								}
							}
							if d.Name() == "rvc" {
								var schema struct {
									Properties map[string]json.RawMessage `json:"properties"`
									Required   []string                   `json:"required"`
								}
								if err := json.Unmarshal(cap.InputSchema, &schema); err != nil || len(schema.Properties) != 3 || schema.Properties["artifact_id"] == nil || schema.Properties["voice_profile_id"] == nil || schema.Properties["title"] == nil || len(schema.Required) != 2 || schema.Required[0] != "artifact_id" || schema.Required[1] != "voice_profile_id" {
									t.Fatal("rvc conversion must require only artifact_id and voice_profile_id, with optional title")
								}
							}
							if d.Name() == "comfyui" {
								var schema struct {
									Properties map[string]json.RawMessage `json:"properties"`
									Required   []string                   `json:"required"`
								}
								if err := json.Unmarshal(cap.InputSchema, &schema); err != nil {
									t.Fatal(err)
								}
								switch cap.ID {
								case "comfyui.workflows":
									if len(schema.Properties) != 0 || len(schema.Required) != 0 {
										t.Fatal("comfyui workflow inventory must take no arguments")
									}
								case "comfyui.run":
									if len(schema.Properties) != 3 || schema.Properties["workflow_id"] == nil || schema.Properties["inputs"] == nil || schema.Properties["title"] == nil || len(schema.Required) != 1 || schema.Required[0] != "workflow_id" {
										t.Fatal("comfyui run must require only workflow_id with optional typed inputs and title")
									}
								case "comfyui.job":
									if len(schema.Properties) != 2 || schema.Properties["job_id"] == nil || schema.Properties["action"] == nil || len(schema.Required) != 2 || schema.Required[0] != "job_id" || schema.Required[1] != "action" {
										t.Fatal("comfyui job must require only job_id and action")
									}
								}
							}
							if d.Name() == "yue" {
								var schema struct {
									Properties map[string]json.RawMessage `json:"properties"`
									Required   []string                   `json:"required"`
								}
								if err := json.Unmarshal(cap.InputSchema, &schema); err != nil {
									t.Fatal(err)
								}
								switch cap.ID {
								case "yue.generate":
									for _, name := range []string{"mode", "style", "lyrics", "duration_seconds", "min_duration_seconds", "text_guidance_scale", "seed", "vocal_gender", "planning_mode", "reference_mode", "sampling", "reference_artifact_id"} {
										if schema.Properties[name] == nil {
											t.Fatal("YuE generate missing input", name)
										}
									}
									if len(schema.Required) != 4 || schema.Required[0] != "mode" || schema.Required[1] != "style" || schema.Required[2] != "lyrics" || schema.Required[3] != "duration_seconds" {
										t.Fatal("YuE generate must require mode/style/lyrics/duration_seconds")
									}
								case "yue.job":
									if len(schema.Properties) != 2 || schema.Properties["job_id"] == nil || schema.Properties["action"] == nil || len(schema.Required) != 2 || schema.Required[0] != "job_id" || schema.Required[1] != "action" {
										t.Fatal("YuE job must require only job_id and action")
									}
								}
							}
							if d.Name() == "station-speech" {
								var schema struct {
									Properties map[string]json.RawMessage `json:"properties"`
									Required   []string                   `json:"required"`
								}
								if err := json.Unmarshal(cap.InputSchema, &schema); err != nil {
									t.Fatal(err)
								}
								switch cap.ID {
								case "station-speech.voices":
									if len(schema.Properties) != 0 || len(schema.Required) != 0 {
										t.Fatal("station speech voice inventory must take no arguments")
									}
								case "station-speech.synthesize":
									if len(schema.Properties) != 4 || schema.Properties["voice_id"] == nil || schema.Properties["text"] == nil || schema.Properties["speed"] == nil || schema.Properties["text_language"] == nil || len(schema.Required) != 2 || schema.Required[0] != "voice_id" || schema.Required[1] != "text" {
										t.Fatal("station speech synthesis must require only voice_id and text, with optional speed and text_language")
									}
								case "station-speech.job":
									if len(schema.Properties) != 2 || schema.Properties["job_id"] == nil || schema.Properties["action"] == nil || len(schema.Required) != 2 || schema.Required[0] != "job_id" || schema.Required[1] != "action" {
										t.Fatal("station speech recovery must require only job_id and action")
									}
								}
							}
							delete(want, cap.ID)
						} else if cap.Implementation != "migration-pending" || cap.Discoverable {
							t.Fatal("future capability exposed", cap.ID)
						}
					}
					if len(want) != 0 {
						t.Fatal("missing implemented capability", want)
					}
				}
			} else if p.Status != "migration-pending" {
				t.Fatal("unimplemented capability claimed ready")
			}
		})
	}
}
func TestPackageRejectsTampering(t *testing.T) {
	src := "../../plugins/speech"
	dir := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		return os.WriteFile(target, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("modified"), 0600)
	if _, err = Verify(dir); err == nil {
		t.Fatal("modified file accepted")
	}
}
func TestPermissionScopeAndRecovery(t *testing.T) {
	p := Package{ID: "speech", Version: "1", Status: "ready", Permissions: []string{"artifact.read"}}
	c := Invocation{Account: "a", Thread: "t", Requester: "u", Purpose: "synthetic", RequestKey: "r"}
	g := Grant{Account: "a", Plugin: "speech", Version: "1", Permissions: []string{"artifact.read"}}
	if e := Authorize(p, g, c, []string{"artifact.read"}); e != nil {
		t.Fatal(e)
	}
	g.Account = "b"
	if Authorize(p, g, c, nil) == nil {
		t.Fatal("cross account accepted")
	}
	g.Account = "a"
	if Authorize(p, g, c, []string{"network"}) == nil {
		t.Fatal("permission escalation")
	}
	p.Status = "migration-pending"
	if Authorize(p, g, c, nil) == nil {
		t.Fatal("pending implementation executed")
	}
	if (Job{State: "running"}).Recovery() != "needs_review" || (Job{State: "running", RemoteID: "remote"}).Recovery() != "query" {
		t.Fatal("unsafe recovery")
	}
}
