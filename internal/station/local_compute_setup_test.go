package station

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the public state through real registration readers, not a mock of
// the reason mapper. No backend process, model or platform is contacted.
func TestLocalComputeStateDistinguishesDeploymentFailures(t *testing.T) {
	for _, id := range []string{localComfyPackage, localYuEPackage} {
		for _, failure := range []struct{ name, want string }{
			{"missing", "尚未登记并授权"},
			{"corrupt", "格式或参数无效"},
			{"directory", "无法读取"},
			{"source_missing", "运行文件缺失"},
			{"source_changed", "完整性校验失败"},
		} {
			t.Run(id+"/"+failure.name, func(t *testing.T) {
				var data, home, registration, entry, python string
				f := newLocalHomeFixture(t, filepath.Join(t.TempDir(), "host"), filepath.Join(t.TempDir(), "home"))
				f.login()
				if code, _ := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != 200 {
					t.Fatalf("initialize: %d", code)
				}
				s := f.handler.chat.projectTools
				if id == localComfyPackage {
					var r localComfyRegistration
					data, home, r = writeLocalComfyFixture(t)
					registration = filepath.Join(data, localComfyRegistrationFile)
					entry, python = filepath.Join(r.Runtime, r.Entry), filepath.Join(r.Runtime, r.Python)
					s.comfy = newLocalComfyServiceAt(data, home)
				} else {
					var r localYuERegistration
					data, home, r = writeLocalYuEFixture(t)
					registration = filepath.Join(data, localYuERegistrationFile)
					entry, python = filepath.Join(r.Runtime, r.Entry), filepath.Join(r.Runtime, r.Python)
					s.yue = newLocalYuEServiceAt(data, home)
				}
				var err error
				switch failure.name {
				case "missing":
					err = os.Remove(registration)
				case "corrupt":
					err = os.WriteFile(registration, []byte(`{"private":"DO-NOT-DISCLOSE"`), 0600)
				case "directory":
					if err = os.Remove(registration); err == nil {
						err = os.Mkdir(registration, 0700)
					}
				case "source_missing":
					err = os.Remove(python)
				case "source_changed":
					err = os.WriteFile(entry, []byte("changed runtime"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				view := localProjectToolByID(t, f, id)
				reason, _ := view["reason"].(string)
				if view["runtime_ready"] != false || !strings.Contains(reason, failure.want) {
					t.Fatalf("wrong state: %#v", view)
				}
				if strings.Contains(reason, data) || strings.Contains(reason, "DO-NOT-DISCLOSE") {
					t.Fatalf("private registration leaked: %q", reason)
				}
			})
		}
	}
}

func TestLocalComputeUnknownFailureDoesNotExposeDetails(t *testing.T) {
	for _, id := range []string{localComfyPackage, localYuEPackage} {
		reason := localComputeSetupReason(id, errors.New(`open E:\private\token.json: sensitive-value`))
		if strings.Contains(reason, "private") || strings.Contains(reason, "token") || strings.Contains(reason, "sensitive") {
			t.Fatalf("private error disclosed: %q", reason)
		}
	}
}
