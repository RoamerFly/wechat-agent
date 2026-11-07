package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func idleConfigFixture(t *testing.T) (*Store, InstalledPackage) {
	t.Helper()
	s, err := OpenStore(t.TempDir(), "synthetic-owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p, err := s.Prepare(t.Context(), readyFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.Install(t.Context(), p.Token, p.Digest, p.Package.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveConfiguration(t.Context(), i.Package.ID, ConfigUpdate{Values: map[string]json.RawMessage{"backend": json.RawMessage(`"old"`)}}); err != nil {
		t.Fatal(err)
	}
	i, err = s.SetEnabled(t.Context(), i.Package.ID, i.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	return s, i
}

func TestSaveIdleConfigurationPreservesEnableIntentAndOriginalAPI(t *testing.T) {
	s, i := idleConfigFixture(t)
	before, err := s.Configuration(t.Context(), i.Package.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := ConfigUpdate{Revision: before.Revision, Values: map[string]json.RawMessage{"backend": json.RawMessage(`"new"`)}}
	if _, err := s.SaveConfiguration(t.Context(), i.Package.ID, input); err == nil {
		t.Fatal("ordinary API accepted enabled tool")
	}
	if _, err := s.SaveIdleConfiguration(t.Context(), i.Package.ID, ConfigUpdate{Revision: before.Revision + 1}); !errors.Is(err, ErrRevision) {
		t.Fatal("stale update accepted", err)
	}
	if _, err := s.SaveIdleConfiguration(t.Context(), i.Package.ID, ConfigUpdate{Revision: before.Revision, Values: map[string]json.RawMessage{"unknown": json.RawMessage(`1`)}}); err == nil {
		t.Fatal("schema validation bypassed")
	}
	after, err := s.SaveIdleConfiguration(t.Context(), i.Package.ID, input)
	if err != nil || after.Revision <= before.Revision || string(after.Values["backend"]) != `"new"` {
		t.Fatalf("idle config: %+v %v", after, err)
	}
	installed, err := s.Installed(t.Context(), i.Package.ID)
	if err != nil || !installed.Enabled || installed.Revision != i.Revision || installed.Digest != i.Digest {
		t.Fatalf("install intent changed: %+v %v", installed, err)
	}
}

func TestSaveIdleConfigurationRejectsPendingWorkAndMigrationAtomically(t *testing.T) {
	for _, kind := range []string{"pending", "submitting", "running", "needs_review", "migration"} {
		t.Run(kind, func(t *testing.T) {
			s, i := idleConfigFixture(t)
			before, err := s.Configuration(t.Context(), i.Package.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "migration" {
				_, err = s.db.ExecContext(context.Background(), `INSERT INTO migrations(id,plugin,digest,source_hash,config_revision,state,created,updated) VALUES('synthetic',?,?,?,?,'pending','t','t')`, i.Package.ID, i.Digest, "synthetic-source", before.Revision)
			} else {
				_, err = s.db.ExecContext(context.Background(), `INSERT INTO jobs(id,plugin,thread,request_key,fingerprint,record,revision,state) VALUES('synthetic',?,'synthetic','synthetic','synthetic','{}',1,?)`, i.Package.ID, kind)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveIdleConfiguration(t.Context(), i.Package.ID, ConfigUpdate{Revision: before.Revision, Values: map[string]json.RawMessage{"backend": json.RawMessage(`"new"`)}}); err == nil {
				t.Fatal("unfinished state allowed config change")
			}
			after, err := s.Configuration(t.Context(), i.Package.ID)
			if err != nil || after.Revision != before.Revision || string(after.Values["backend"]) != `"old"` {
				t.Fatal("rejected update altered configuration", after, err)
			}
			var count int
			table := "jobs"
			if kind == "migration" {
				table = "migrations"
			}
			if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE plugin=?", i.Package.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("unfinished record was discarded", count, err)
			}
		})
	}
}
