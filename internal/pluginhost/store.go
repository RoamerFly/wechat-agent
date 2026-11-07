package pluginhost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is private to one account. Preparing a package neither executes it nor
// grants permissions. Installation commits an immutable snapshot and defaults
// to disabled; approval is supplied only by the authenticated admin control plane.
type Store struct {
	mu           sync.Mutex
	dir, account string
	db           *sql.DB
}
type Prepared struct {
	Token   string  `json:"token"`
	Digest  string  `json:"digest"`
	Package Package `json:"package"`
	Created string  `json:"created"`
}
type InstalledPackage struct {
	Package     Package  `json:"package"`
	Digest      string   `json:"digest"`
	Permissions []string `json:"permissions"`
	Enabled     bool     `json:"enabled"`
	Removed     bool     `json:"removed"`
	Revision    int      `json:"revision"`
}

func OpenStore(dir, account string) (*Store, error) {
	if account == "" {
		return nil, errors.New("account required")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(absolute, "plugins.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	for _, query := range []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS owner(singleton INTEGER PRIMARY KEY CHECK(singleton=1),account TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS preparations(token TEXT PRIMARY KEY,digest TEXT NOT NULL,manifest BLOB NOT NULL,created TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS installed(id TEXT PRIMARY KEY,digest TEXT NOT NULL,manifest BLOB NOT NULL,permissions BLOB NOT NULL,enabled INTEGER NOT NULL DEFAULT 0,removed INTEGER NOT NULL DEFAULT 0,revision INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS audit(seq INTEGER PRIMARY KEY,at TEXT NOT NULL,plugin TEXT NOT NULL,action TEXT NOT NULL,digest TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS configurations(plugin TEXT PRIMARY KEY,revision INTEGER NOT NULL,values_json BLOB NOT NULL,secrets_json BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS configuration_versions(plugin TEXT NOT NULL,revision INTEGER NOT NULL,values_json BLOB NOT NULL,secrets_json BLOB NOT NULL,PRIMARY KEY(plugin,revision))`,
		`CREATE TABLE IF NOT EXISTS migrations(id TEXT PRIMARY KEY,plugin TEXT NOT NULL,digest TEXT NOT NULL,source_hash TEXT NOT NULL,config_revision INTEGER NOT NULL,permissions_hash TEXT NOT NULL DEFAULT '',config_hash TEXT NOT NULL DEFAULT '',installed_revision INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL,created TEXT NOT NULL,updated TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS jobs(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,plugin TEXT NOT NULL,thread TEXT NOT NULL,request_key TEXT NOT NULL,fingerprint TEXT NOT NULL,record BLOB NOT NULL,revision INTEGER NOT NULL,state TEXT NOT NULL,UNIQUE(plugin,thread,request_key))`,
		`CREATE INDEX IF NOT EXISTS jobs_plugin_sequence ON jobs(plugin,seq)`,
		// Application hosts may keep several durable tool ledgers under one
		// application plugin. Filter inside SQLite so a publication page is not
		// consumed by unrelated interaction records before LIMIT is applied.
		`CREATE INDEX IF NOT EXISTS jobs_plugin_tool_sequence ON jobs(plugin,json_extract(CAST(record AS TEXT),'$.tool'),seq)`,
		`CREATE TABLE IF NOT EXISTS job_private_results(job_id TEXT PRIMARY KEY,revision INTEGER NOT NULL,payload BLOB NOT NULL)`,
		// A live session is a durable host coordination record. This Store is
		// account-scoped, so the partial unique index below gives each fish one
		// active session even when more than one controller has opened the DB.
		`CREATE TABLE IF NOT EXISTS live_sessions(id TEXT PRIMARY KEY,request_key TEXT NOT NULL UNIQUE,fingerprint TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision >= 1),active INTEGER NOT NULL CHECK(active IN (0,1)),payload BLOB NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS live_sessions_one_active ON live_sessions(active) WHERE active=1`,
		// Host state is intentionally separate from plugin configuration and
		// runtime data. It remains private to this account's host Store even if a
		// package is disabled, removed, or never installed.
		`CREATE TABLE IF NOT EXISTS host_states(plugin TEXT NOT NULL,namespace TEXT NOT NULL,revision INTEGER NOT NULL,source_hash TEXT NOT NULL,PRIMARY KEY(plugin,namespace))`,
		`CREATE TABLE IF NOT EXISTS host_state_records(plugin TEXT NOT NULL,namespace TEXT NOT NULL,key TEXT NOT NULL,payload BLOB NOT NULL,PRIMARY KEY(plugin,namespace,key),FOREIGN KEY(plugin,namespace) REFERENCES host_states(plugin,namespace) ON DELETE CASCADE)`,
	} {
		if _, err = db.Exec(query); err != nil {
			return fail(err)
		}
	}
	// Forward-compatible for stores created by an earlier local preview build.
	for _, query := range []string{`ALTER TABLE migrations ADD COLUMN permissions_hash TEXT NOT NULL DEFAULT ''`, `ALTER TABLE migrations ADD COLUMN config_hash TEXT NOT NULL DEFAULT ''`, `ALTER TABLE migrations ADD COLUMN installed_revision INTEGER NOT NULL DEFAULT 0`} {
		_, _ = db.Exec(query)
	}
	if _, err = db.Exec(`INSERT OR IGNORE INTO owner(singleton,account) VALUES(1,?)`, account); err != nil {
		return fail(err)
	}
	var owner string
	if err = db.QueryRow(`SELECT account FROM owner WHERE singleton=1`).Scan(&owner); err != nil {
		return fail(err)
	}
	if owner != account {
		return fail(errors.New("plugin store belongs to another account"))
	}
	return &Store{dir: absolute, account: account, db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// snapshot copies through an os.Root handle so links cannot escape the source
// tree even if a directory changes during copying. It validates the destination,
// never installs directly from the mutable source path.
func snapshot(ctx context.Context, source, destination string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("package links forbidden")
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		input, e := root.Open(name)
		if e != nil {
			return e
		}
		defer input.Close()
		info, e := input.Stat()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("special package file forbidden")
		}
		output, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(output, input)
		if e == nil {
			e = output.Sync()
		}
		closeErr := output.Close()
		if e != nil {
			return e
		}
		return closeErr
	})
}
func (s *Store) Prepare(ctx context.Context, source string) (Prepared, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Prepared
	sourceRoot, err := filepath.EvalSymlinks(source)
	if err != nil {
		return result, err
	}
	sourceRoot, err = filepath.Abs(sourceRoot)
	if err != nil {
		return result, err
	}
	storeRoot, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		return result, err
	}
	relative, err := filepath.Rel(sourceRoot, storeRoot)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return result, errors.New("source cannot contain plugin store")
	}
	stage, err := os.MkdirTemp(s.dir, ".prepare-")
	if err != nil {
		return result, err
	}
	// stage is an absolute, newly created child of this store, never caller input.
	defer os.RemoveAll(stage)
	if err = snapshot(ctx, source, stage); err != nil {
		return result, err
	}
	return s.prepareStage(ctx, stage)
}

// Caller owns the store lock and a private, completed snapshot. Directory and
// archive imports share exactly the same verification and approval record.
func (s *Store) prepareStage(ctx context.Context, stage string) (Prepared, error) {
	var result Prepared
	p, err := Verify(stage)
	if err != nil {
		return result, err
	}
	raw, err := os.ReadFile(filepath.Join(stage, "plugin.json"))
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	packages := filepath.Join(s.dir, "packages")
	if err = os.MkdirAll(packages, 0700); err != nil {
		return result, err
	}
	target := filepath.Join(packages, digest)
	if _, err = os.Stat(target); errors.Is(err, os.ErrNotExist) {
		err = os.Rename(stage, target)
	} else if err == nil {
		_, err = Verify(target)
	}
	if err != nil {
		return result, err
	}
	token := make([]byte, 24)
	if _, err = rand.Read(token); err != nil {
		return result, err
	}
	result = Prepared{Token: hex.EncodeToString(token), Digest: digest, Package: p, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	_, err = s.db.ExecContext(ctx, `INSERT INTO preparations(token,digest,manifest,created) VALUES(?,?,?,?)`, result.Token, digest, raw, result.Created)
	return result, err
}

func (s *Store) Install(ctx context.Context, token, digest string, granted []string) (InstalledPackage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result InstalledPackage
	var raw []byte
	var expected string
	if err := s.db.QueryRowContext(ctx, `SELECT digest,manifest FROM preparations WHERE token=?`, token).Scan(&expected, &raw); err != nil {
		return result, errors.New("prepared package not found")
	}
	if digest != expected {
		return result, errors.New("approval digest changed")
	}
	p, err := Verify(filepath.Join(s.dir, "packages", expected))
	if err != nil {
		return result, err
	}
	actual, err := os.ReadFile(filepath.Join(s.dir, "packages", expected, "plugin.json"))
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(actual)
	if hex.EncodeToString(sum[:]) != expected {
		return result, errors.New("prepared manifest changed")
	}
	if p.Status != "ready" {
		return result, errors.New("plugin implementation not ready for installation")
	}
	declared := map[string]bool{}
	for _, v := range p.Permissions {
		declared[v] = true
	}
	seen := map[string]bool{}
	for _, v := range granted {
		if !declared[v] || seen[v] {
			return result, errors.New("invalid approval permissions")
		}
		seen[v] = true
	}
	permissions, err := json.Marshal(granted)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var enabled, revision int
	var previous string
	var previousManifest []byte
	err = tx.QueryRowContext(ctx, `SELECT digest,manifest,enabled,revision FROM installed WHERE id=?`, p.ID).Scan(&previous, &previousManifest, &enabled, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if enabled != 0 {
		return result, errors.New("disable and drain plugin before installation")
	}
	if previous != "" && previous != expected {
		if err = s.validateUpgradeConfiguration(ctx, tx, previous, previousManifest, p, expected); err != nil {
			return result, err
		}
	}
	revision++
	_, err = tx.ExecContext(ctx, `INSERT INTO installed(id,digest,manifest,permissions,enabled,removed,revision) VALUES(?,?,?,?,0,0,?) ON CONFLICT(id) DO UPDATE SET digest=excluded.digest,manifest=excluded.manifest,permissions=excluded.permissions,enabled=0,removed=0,revision=excluded.revision`, p.ID, expected, raw, permissions, revision)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit(at,plugin,action,digest) VALUES(?,?,?,?)`, time.Now().UTC().Format(time.RFC3339Nano), p.ID, "installed_disabled", expected); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM preparations WHERE token=?`, token); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	result = InstalledPackage{Package: p, Digest: expected, Permissions: append([]string{}, granted...), Revision: revision}
	return result, nil
}
func (s *Store) List(ctx context.Context) ([]InstalledPackage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT digest,manifest,permissions,enabled,removed,revision FROM installed ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []InstalledPackage{}
	for rows.Next() {
		var item InstalledPackage
		var p, g []byte
		if err = rows.Scan(&item.Digest, &p, &g, &item.Enabled, &item.Removed, &item.Revision); err != nil {
			return nil, err
		}
		if json.Unmarshal(p, &item.Package) != nil || json.Unmarshal(g, &item.Permissions) != nil {
			return nil, errors.New("invalid installed plugin data")
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) PackageDirectory(digest string) (string, error) {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return "", errors.New("invalid package digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("invalid package digest")
	}
	return filepath.Join(s.dir, "packages", digest), nil
}
