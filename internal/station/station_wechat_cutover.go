package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/workspacefs"
)

const legacyStopMarkerFile = "legacy-cutover-state.json"

type legacyCutover struct {
	LegacyHost     string `json:"legacy_host"`
	NativeProfile  string `json:"native_profile"`
	TargetLogin    string `json:"target_login"`
	TargetIdentity string `json:"target_identity"`
}

type legacyStopMarker struct {
	Version        int       `json:"version"`
	State          string    `json:"state"`
	Mode           string    `json:"mode,omitempty"`
	Epoch          string    `json:"epoch"`
	AccountID      string    `json:"account_id"`
	DataRoot       string    `json:"data_root"`
	NativeServerID string    `json:"native_server_id"`
	At             time.Time `json:"at"`
}

func (m legacyStopMarker) validMode() bool {
	return m.Mode == "" || m.Version == 2 && m.Mode == "offline_hold"
}

func cutoverPath(path string) string {
	path = filepath.FromSlash(path)
	if runtime.GOOS == "windows" {
		const unc = `\\?\UNC\`
		if len(path) >= len(unc) && strings.EqualFold(path[:len(unc)], unc) {
			return `\\` + path[len(unc):]
		}
		path = strings.TrimPrefix(path, `\\?\`)
		if strings.HasPrefix(path, `\\.\`) {
			return ""
		}
	}
	return path
}

func sameCutoverPath(first, second string) bool {
	first, second = cutoverPath(first), cutoverPath(second)
	if !filepath.IsAbs(first) || !filepath.IsAbs(second) || first != filepath.Clean(first) || second != filepath.Clean(second) {
		return false
	}
	return first == second || runtime.GOOS == "windows" && strings.EqualFold(first, second)
}

func (s *stationWechat) legacyHostEvidence(snapshot *os.Root, source legacyImportSource, oldServer string) (string, [32]byte) {
	var noDigest [32]byte
	path := cutoverPath(source.LegacyHostRoot)
	if snapshot == nil || !filepath.IsAbs(path) || path != filepath.Clean(path) ||
		legacyImportOverlap(path, source.Root) || legacyImportOverlap(path, s.dataDir) || legacyImportOverlap(path, s.workspaceRoot) {
		return "pending", noDigest
	}
	copied, err := source.guard.readFile(snapshot, legacyStopMarkerFile, 16<<10)
	var marker legacyStopMarker
	if err != nil || json.Unmarshal(copied, &marker) != nil || (marker.Version != 1 && marker.Version != 2) ||
		marker.State != "stopped" || !marker.validMode() || len(marker.Epoch) != 32 || strings.Trim(marker.Epoch, "0123456789abcdef") != "" ||
		marker.AccountID != source.Primary || !sameCutoverPath(marker.DataRoot, path) ||
		!identity.ValidPathID(marker.NativeServerID) || oldServer != "" && marker.NativeServerID != oldServer ||
		marker.At.IsZero() || marker.At.After(time.Now().Add(time.Minute)) {
		return "pending", noDigest
	}
	root, err := workspacefs.OpenPath(path)
	if err != nil {
		return "changed", noDigest
	}
	defer root.Close()
	current, err := readLegacyImportFile(root, legacyStopMarkerFile, 16<<10)
	if err != nil || !bytes.Equal(copied, current) {
		return "changed", sha256.Sum256(current)
	}
	return "stop_record_matched", sha256.Sum256(current)
}

func (s *stationWechat) nativeImportReceipt(id, account string) (string, [32]byte) {
	var noDigest [32]byte
	root, err := workspacefs.OpenPath(filepath.Dir(s.root))
	if err != nil {
		return "pending", noDigest
	}
	defer root.Close()
	raw, err := readLegacyImportFile(root, "native-import-"+id+".json", 32<<10)
	if errors.Is(err, os.ErrNotExist) {
		return "pending", noDigest
	}
	digest := sha256.Sum256(raw)
	var receipt struct {
		ID          string `json:"id"`
		StorageMode string `json:"storageMode"`
		ProfilePath string `json:"profilePath"`
	}
	profile := filepath.Join("native", "accounts", account, "profile")
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.ID != account || receipt.StorageMode != "managed" ||
		!sameCutoverPath(receipt.ProfilePath, filepath.Join(filepath.Dir(s.root), profile)) {
		return "mismatch", digest
	}
	dir, err := workspacefs.OpenExistingDirectory(root, profile)
	if err != nil {
		return "mismatch", digest
	}
	dir.Close()
	return "import_receipt_matched", digest
}

func (s *stationWechat) nativeImportEvidence(id, account string, status linuxnative.Status) (string, [32]byte) {
	profile, digest := s.nativeImportReceipt(id, account)
	if profile != "import_receipt_matched" {
		return profile, digest
	}
	if status.AccountID == "" || status.StorageMode == "" {
		return "pending", digest
	}
	if status.AccountID != account || status.StorageMode != "managed" {
		return "mismatch", digest
	}
	return profile, digest
}

func (s *stationWechat) checkLegacyCutover(ctx context.Context, id string, native *linuxnative.Sender, snapshot *os.Root, source legacyImportSource, oldServer string) (*legacyCutover, [2][32]byte) {
	legacy, marker := s.legacyHostEvidence(snapshot, source, oldServer)
	status, _ := native.Status(ctx)
	profile, receipt := s.nativeImportEvidence(id, native.Account(), status)
	login := targetLoginState(status, native.Account())
	return &legacyCutover{
		LegacyHost: legacy, NativeProfile: profile, TargetLogin: login, TargetIdentity: "owner_check_pending",
	}, [2][32]byte{marker, receipt}
}

func targetLoginState(status linuxnative.Status, account string) string {
	login := "pending"
	if status.AccountID == account && status.LoginObserved {
		switch {
		case !status.Authenticated:
			login = "signed_out"
		case status.Connection == "online" || status.Connection == "connected":
			login = "online"
		default:
			login = "authenticated_offline"
		}
	}
	return login
}
