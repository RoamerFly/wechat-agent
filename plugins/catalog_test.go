package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestContractForWebSearchPreservesExactManifestDigestAndDeclarations(t *testing.T) {
	contract, err := ContractFor("web-search")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := declarations.ReadFile("web-search/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if contract.ManifestSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("manifest digest = %q, want exact bytes digest", contract.ManifestSHA256)
	}
	backend, backendOK := contract.Configuration.Properties["search_backend"]
	key, keyOK := contract.Configuration.Properties["search_key"]
	if !backendOK || backend.Type != "string" || !keyOK || !key.Secret || !key.HostOnly {
		t.Fatalf("web search configuration declaration missing: %#v", contract.Configuration.Properties)
	}
	if len(contract.UI) == 0 || len(contract.Capabilities) == 0 {
		t.Fatal("declarative UI or capabilities were not embedded")
	}
}

func TestContractForRejectsUnsafeOrUnknownID(t *testing.T) {
	for _, id := range []string{"", "../web-search", "not-installed"} {
		if _, err := ContractFor(id); err == nil {
			t.Fatal("expected unavailable contract for", id)
		}
	}
}

func TestContractForOBSPreviewDeclaresOnlyLoopbackConfiguration(t *testing.T) {
	contract, err := ContractFor("obs-preview")
	if err != nil {
		t.Fatal(err)
	}
	if contract.Package.Runtime.Transport != "http-adapter" || contract.Package.Runtime.Entrypoint != "runtime/managed-service.json" {
		t.Fatalf("OBS preview runtime = %#v", contract.Package.Runtime)
	}
	if len(contract.Package.Permissions) != 1 || contract.Package.Permissions[0] != "network" {
		t.Fatalf("OBS preview permissions = %#v", contract.Package.Permissions)
	}
	port, portOK := contract.Configuration.Properties["port"]
	password, passwordOK := contract.Configuration.Properties["password"]
	if !portOK || port.Type != "integer" || string(port.Default) != "4455" || port.Minimum == nil || *port.Minimum != 1 || port.Maximum == nil || *port.Maximum != 65535 {
		t.Fatalf("OBS preview port declaration = %#v", port)
	}
	if !passwordOK || password.Type != "string" || !password.Secret || !password.HostOnly {
		t.Fatalf("OBS preview password declaration = %#v", password)
	}
	if _, present := contract.Configuration.Properties["host"]; present {
		t.Fatal("OBS preview must not declare a remote host")
	}
	if len(contract.Capabilities) != 1 || contract.Capabilities[0].ID != "obs.preview" || contract.Capabilities[0].Discoverable || len(contract.Capabilities[0].Permissions) != 1 || contract.Capabilities[0].Permissions[0] != "network" {
		t.Fatalf("OBS preview capability = %#v", contract.Capabilities)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(contract.Capabilities[0].InputSchema, &schema); err != nil || len(schema.Required) != 1 || schema.Required[0] != "action" || len(schema.Properties["action"].Enum) != 3 || schema.Properties["action"].Enum[0] != "start" || schema.Properties["action"].Enum[1] != "frame" || schema.Properties["action"].Enum[2] != "stop" {
		t.Fatalf("OBS preview action schema = %s", contract.Capabilities[0].InputSchema)
	}
}
