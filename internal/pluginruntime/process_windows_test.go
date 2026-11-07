//go:build windows

package pluginruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWindowsBrokerLaunchEnvelope(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"pythonw.exe", "windows_plugin_host.pyw", "windows_lpac.py"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, env := range []map[string]string{nil, {}, {"DOCUMENT_RUNTIME_ROOT": dir}} {
		spec := LaunchSpec{DataDir: dir, Pythonw: filepath.Join(dir, "pythonw.exe"), WindowsHelper: filepath.Join(dir, "windows_plugin_host.pyw"), Environment: env}
		cmd, cleanup, err := isolatedCommand(spec, []string{"fixture"})
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(cmd.Args[2])
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if len(env) > 0 {
			var got map[string]string
			expected := map[string]string{"DOCUMENT_RUNTIME_ROOT": WindowsProcessPath(dir)}
			if json.Unmarshal(fields["environment"], &got) != nil || !reflect.DeepEqual(got, expected) || env["DOCUMENT_RUNTIME_ROOT"] != dir {
				t.Fatal("missing plugin environment")
			}
			delete(fields, "environment")
		}
		for _, key := range []string{"argv", "cwd", "read_only", "writable", "parent_pid"} {
			if _, ok := fields[key]; !ok {
				t.Fatalf("missing broker field %s", key)
			}
			delete(fields, key)
		}
		if len(fields) != 0 {
			t.Fatal("unexpected fields rejected by strict brokers", fields)
		}
	}
}
