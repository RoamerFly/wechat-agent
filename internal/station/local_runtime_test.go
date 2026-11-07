package station

import (
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/agenthost"
)

func TestLocalRuntimeCannotBeEditedByTheFish(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	windows := &agenthost.WindowsConfig{Pythonw: filepath.Join(workspace, "pythonw.exe"), Helper: filepath.Join(base, "helper.pyw"), Spool: filepath.Join(base, "spool")}
	if handler, err := NewLocalHomeHandler(filepath.Join(base, "private"), workspace, "synthetic-password", "", windows); err == nil {
		handler.Close()
		t.Fatal("mutable host interpreter admitted")
	} else if !strings.Contains(err.Error(), "工作空间") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}
