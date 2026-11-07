package admin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/workspacefs"
)

// fishPrivateRoot is the service-owned account-purge boundary below a
// persistent fish home. Callers must still derive a verified private child
// scope; this helper never grants the returned parent as an Agent mount.
func fishPrivateRoot(managedRoot, fishID string) string {
	// fishID has already been validated against the registry. Keep a compact
	// physical key alongside the full ID kept in bindings and process records.
	return filepath.Join(managedRoot, "fish_home", fishID[:len("fish-")+32], "private")
}

// 128-bit directory keys leave room for the script name in Windows working
// paths. Authorization always uses the full account and sender identities.
func fishAccountDir(account string) string { return accountDirectory(account)[:32] }

// boundFishHome resolves the caller's private mount inside the locally owned
// fish home. The fish identity gives persistence, not permission to read the
// diary of every bound account or owner.
func (p *permittedTools) boundFishHome() (fishHome, fishID string, err error) {
	if p == nil || p.manager == nil || p.manager.fish == nil ||
		!p.workspaceExecutionAllowed() || !p.privileged() ||
		p.trace.Account != p.manager.native.Account() || p.trace.Sender == "" ||
		p.trace.Thread != p.trace.Sender || p.trace.IsGroup || strings.HasSuffix(p.trace.Thread, "@chatroom") {
		return "", "", errors.New("fish_home_scope_denied")
	}
	fishID, bound, readErr := p.manager.fish.Bindings()
	if readErr != nil || !fishidentity.ValidFishID(fishID) || !bound[p.trace.Account] {
		return "", "", errors.New("fish_home_scope_denied")
	}
	// The physical fish directory is shared only as a parent. The actual host
	// grant is private to the verified account and its explicit owner thread.
	base := fishPrivateRoot(p.manager.managedRoot(), fishID)
	// Keep an account-purge boundary below the shared physical fish directory;
	// the final scope is still tied to the verified explicit private owner.
	ownerDir := companion.ScopeID("wechat", p.trace.Account, p.trace.Sender)[:32]
	fishHome = filepath.Join(base, fishAccountDir(p.trace.Account), ownerDir)
	// LPAC inherits this as TEMP/TMP. Create it through the host's pinned-path
	// API now: Python's recursive makedirs falls back to legacy path handling
	// for a deeply nested Windows fish-home root.
	root, ensureErr := workspacefs.EnsurePath(fishHome)
	if ensureErr != nil {
		return "", "", errors.New("fish_home_unavailable")
	}
	if closeErr := root.Close(); closeErr != nil {
		return "", "", errors.New("fish_home_unavailable")
	}
	scratch, scratchErr := workspacefs.EnsurePath(filepath.Join(fishHome, "scratch"))
	if scratchErr != nil || scratch.Close() != nil {
		return "", "", errors.New("fish_home_unavailable")
	}
	// EnsurePath pins ancestors while creating/opening them. Resolve again
	// before a long-lived host mount to reject symlink/reparse aliases.
	resolved, resolveErr := filepath.EvalSymlinks(fishHome)
	info, statErr := os.Lstat(fishHome)
	rel, relErr := filepath.Rel(fishHome, resolved)
	if resolveErr != nil || relErr != nil || rel != "." || statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("fish_home_unavailable")
	}
	return fishHome, fishID, nil
}
