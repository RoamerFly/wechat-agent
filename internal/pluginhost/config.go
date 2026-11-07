package pluginhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// ConfigSchema is the supported declarative subset, not an assertion of full
// JSON Schema compliance. Unsupported keywords fail closed during configuration.
type ConfigSchema struct {
	Schema               string                 `json:"$schema,omitempty"`
	Title                string                 `json:"title,omitempty"`
	Description          string                 `json:"description,omitempty"`
	Type                 string                 `json:"type"`
	Properties           map[string]ConfigField `json:"properties"`
	Required             []string               `json:"required,omitempty"`
	AdditionalProperties bool                   `json:"additionalProperties"`
}
type ConfigField struct {
	Type        string            `json:"type"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Secret      bool              `json:"x-secret,omitempty"`
	HostOnly    bool              `json:"x-host-only,omitempty"`
	Default     json.RawMessage   `json:"default,omitempty"`
	Enum        []json.RawMessage `json:"enum,omitempty"`
	Minimum     *float64          `json:"minimum,omitempty"`
	Maximum     *float64          `json:"maximum,omitempty"`
	MinLength   *int              `json:"minLength,omitempty"`
	MaxLength   *int              `json:"maxLength,omitempty"`
}
type Configuration struct {
	Revision          int                        `json:"revision"`
	Values            map[string]json.RawMessage `json:"values"`
	ConfiguredSecrets []string                   `json:"configured_secrets"`
}
type ConfigUpdate struct {
	Revision     int                        `json:"revision"`
	Values       map[string]json.RawMessage `json:"values"`
	Secrets      map[string]string          `json:"secrets,omitempty"`
	ClearSecrets []string                   `json:"clear_secrets,omitempty"`
}

func (s *Store) Installed(ctx context.Context, id string) (InstalledPackage, error) {
	var item InstalledPackage
	var p, g []byte
	err := s.db.QueryRowContext(ctx, `SELECT digest,manifest,permissions,enabled,removed,revision FROM installed WHERE id=?`, id).Scan(&item.Digest, &p, &g, &item.Enabled, &item.Removed, &item.Revision)
	if err != nil {
		return item, err
	}
	if json.Unmarshal(p, &item.Package) != nil || json.Unmarshal(g, &item.Permissions) != nil {
		return item, errors.New("invalid package record")
	}
	return item, nil
}
func (s *Store) PackageFile(ctx context.Context, id, name string) ([]byte, error) {
	switch name {
	case "config.schema.json", "ui.schema.json", "capabilities.json":
	default:
		return nil, errors.New("package resource not exposed")
	}
	item, err := s.Installed(ctx, id)
	if err != nil {
		return nil, err
	}
	dir, err := s.PackageDirectory(item.Digest)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	raw, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != item.Package.Files[name] {
		return nil, errors.New("package schema changed")
	}
	return raw, nil
}
func (s *Store) ConfigSchema(ctx context.Context, id string) (ConfigSchema, error) {
	raw, err := s.PackageFile(ctx, id, "config.schema.json")
	if err != nil {
		return ConfigSchema{}, err
	}
	return parseConfigSchema(raw)
}

func parseConfigSchema(raw []byte) (ConfigSchema, error) {
	var schema ConfigSchema
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&schema); err != nil {
		return schema, errors.New("unsupported configuration schema")
	}
	if schema.Type != "object" || schema.AdditionalProperties {
		return schema, errors.New("configuration must declare its fields")
	}
	for name, field := range schema.Properties {
		if name == "" || strings.ContainsRune(name, 0) {
			return schema, errors.New("invalid configuration field")
		}
		switch field.Type {
		case "string", "integer", "number", "boolean":
		default:
			return schema, fmt.Errorf("unsupported configuration type: %s", field.Type)
		}
		if field.HostOnly && !field.Secret {
			return ConfigSchema{}, errors.New("host-only configuration must be secret")
		}
		if field.Secret && (field.Type != "string" || len(field.Default) > 0) {
			return schema, errors.New("secret must be string without default")
		}
	}
	for _, name := range schema.Required {
		if _, ok := schema.Properties[name]; !ok {
			return schema, errors.New("required field not declared")
		}
	}
	return schema, nil
}

var ErrConfigurationMigrationRequired = errors.New("plugin upgrade requires configuration migration; existing installation and configuration retained")

// A package replacement never resets configuration. Compatibility is checked
// inside the installation transaction before replacing the registration. The
// original row, revision and private history remain byte-for-byte untouched.
func (s *Store) validateUpgradeConfiguration(ctx context.Context, tx *sql.Tx, previousDigest string, previousManifest []byte, next Package, digest string) error {
	var revision int
	var rawValues, rawSecrets []byte
	err := tx.QueryRowContext(ctx, `SELECT revision,values_json,secrets_json FROM configurations WHERE plugin=?`, next.ID).Scan(&revision, &rawValues, &rawSecrets)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // A never-configured installation still requires setup before activation.
	}
	if err != nil {
		return err
	}
	values := map[string]json.RawMessage{}
	private := map[string]string{}
	if revision <= 0 || json.Unmarshal(rawValues, &values) != nil || json.Unmarshal(rawSecrets, &private) != nil || values == nil || private == nil {
		return ErrConfigurationMigrationRequired
	}
	readSchema := func(p Package, packageDigest string) (ConfigSchema, error) {
		dir, err := s.PackageDirectory(packageDigest)
		if err != nil {
			return ConfigSchema{}, err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return ConfigSchema{}, err
		}
		defer root.Close()
		raw, err := root.ReadFile("config.schema.json")
		if err != nil {
			return ConfigSchema{}, err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != p.Files["config.schema.json"] {
			return ConfigSchema{}, ErrConfigurationMigrationRequired
		}
		return parseConfigSchema(raw)
	}
	schema, err := readSchema(next, digest)
	if err != nil {
		return ErrConfigurationMigrationRequired
	}
	for name, raw := range values {
		field, ok := schema.Properties[name]
		if !ok || field.Secret || validateField(field, raw) != nil {
			return ErrConfigurationMigrationRequired
		}
	}
	var previous Package
	if len(private) > 0 && json.Unmarshal(previousManifest, &previous) != nil {
		return ErrConfigurationMigrationRequired
	}
	var oldSchema ConfigSchema
	if len(private) > 0 {
		oldSchema, err = readSchema(previous, previousDigest)
		if err != nil {
			return ErrConfigurationMigrationRequired
		}
	}
	for name, value := range private {
		field, ok := schema.Properties[name]
		old, wasDeclared := oldSchema.Properties[name]
		if !ok || !field.Secret || !wasDeclared || !old.Secret || old.HostOnly && !field.HostOnly {
			return ErrConfigurationMigrationRequired
		}
		if value != "" {
			raw, _ := json.Marshal(value)
			if validateField(field, raw) != nil {
				return ErrConfigurationMigrationRequired
			}
			values[name] = raw
		}
	}
	for name, field := range schema.Properties {
		if _, present := values[name]; !present && !field.Secret && len(field.Default) > 0 {
			if !json.Valid(field.Default) || validateField(field, field.Default) != nil {
				return ErrConfigurationMigrationRequired
			}
			values[name] = field.Default
		}
	}
	for _, name := range schema.Required {
		if _, present := values[name]; !present {
			return ErrConfigurationMigrationRequired
		}
	}
	if validateConfiguredRoutes(next, values) != nil {
		return ErrConfigurationMigrationRequired
	}
	return nil
}
func validateField(field ConfigField, raw json.RawMessage) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return errors.New("invalid configuration value")
	}
	switch field.Type {
	case "string":
		text, ok := value.(string)
		if !ok {
			return errors.New("expected text")
		}
		n := utf8.RuneCountInString(text)
		if field.MinLength != nil && n < *field.MinLength || field.MaxLength != nil && n > *field.MaxLength {
			return errors.New("text length outside schema")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return errors.New("expected boolean")
		}
	case "integer", "number":
		number, ok := value.(json.Number)
		if !ok {
			return errors.New("expected number")
		}
		n, err := number.Float64()
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return errors.New("invalid number")
		}
		if field.Type == "integer" && math.Trunc(n) != n {
			return errors.New("expected integer")
		}
		if field.Minimum != nil && n < *field.Minimum || field.Maximum != nil && n > *field.Maximum {
			return errors.New("number outside schema")
		}
	}
	if len(field.Enum) > 0 {
		found := false
		for _, candidate := range field.Enum {
			var option any
			d := json.NewDecoder(bytes.NewReader(candidate))
			d.UseNumber()
			if d.Decode(&option) != nil {
				return errors.New("invalid schema option")
			}
			a, _ := json.Marshal(value)
			b, _ := json.Marshal(option)
			if bytes.Equal(a, b) {
				found = true
				break
			}
		}
		if !found {
			return errors.New("value not in schema options")
		}
	}
	return nil
}
func (s *Store) Configuration(ctx context.Context, id string) (Configuration, error) {
	if _, err := s.Installed(ctx, id); err != nil {
		return Configuration{}, err
	}
	c := Configuration{Values: map[string]json.RawMessage{}, ConfiguredSecrets: []string{}}
	var values, secrets []byte
	err := s.db.QueryRowContext(ctx, `SELECT revision,values_json,secrets_json FROM configurations WHERE plugin=?`, id).Scan(&c.Revision, &values, &secrets)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	var private map[string]string
	if json.Unmarshal(values, &c.Values) != nil || json.Unmarshal(secrets, &private) != nil {
		return c, errors.New("invalid stored configuration")
	}
	for key, value := range private {
		if value != "" {
			c.ConfiguredSecrets = append(c.ConfiguredSecrets, key)
		}
	}
	sort.Strings(c.ConfiguredSecrets)
	return c, nil
}
func (s *Store) SaveConfiguration(ctx context.Context, id string, input ConfigUpdate) (Configuration, error) {
	return s.saveConfiguration(ctx, id, input, nil)
}

// SaveIdleConfiguration is a trusted host lifecycle operation. The controller
// must first exclude new invocations and terminate the idle runtime. Unlike the
// ordinary configuration API it preserves enabled intent, while the same commit
// rejects work or migrations that still require recovery. Plugin/model callers
// must not receive this operation as a capability.
func (s *Store) SaveIdleConfiguration(ctx context.Context, id string, input ConfigUpdate) (Configuration, error) {
	return s.saveConfigurationMode(ctx, id, input, nil, true)
}

// saveConfiguration keeps the normal public API semantics while allowing a
// host-internal lifecycle transition to join the exact same SQLite commit.
func (s *Store) saveConfiguration(ctx context.Context, id string, input ConfigUpdate, onCommit func(*sql.Tx, int) error) (Configuration, error) {
	return s.saveConfigurationMode(ctx, id, input, onCommit, false)
}

func (s *Store) saveConfigurationMode(ctx context.Context, id string, input ConfigUpdate, onCommit func(*sql.Tx, int) error, idle bool) (Configuration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.Installed(ctx, id)
	if err != nil {
		return Configuration{}, err
	}
	if item.Removed || item.Enabled && !idle {
		return Configuration{}, errors.New("stop plugin before editing configuration")
	}
	schema, err := s.ConfigSchema(ctx, id)
	if err != nil {
		return Configuration{}, err
	}
	values := map[string]json.RawMessage{}
	for name, raw := range input.Values {
		field, ok := schema.Properties[name]
		if !ok || field.Secret {
			return Configuration{}, errors.New("unknown or private configuration field")
		}
		if !json.Valid(raw) {
			return Configuration{}, errors.New("invalid JSON value")
		}
		if err = validateField(field, raw); err != nil {
			return Configuration{}, fmt.Errorf("configuration field %s: %w", name, err)
		}
		values[name] = raw
	}
	for name, field := range schema.Properties {
		if _, ok := values[name]; !ok && !field.Secret && len(field.Default) > 0 {
			if !json.Valid(field.Default) || validateField(field, field.Default) != nil {
				return Configuration{}, errors.New("invalid schema default")
			}
			values[name] = field.Default
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Configuration{}, err
	}
	defer tx.Rollback()
	if idle {
		var revision, removed int
		if err := tx.QueryRowContext(ctx, `SELECT revision,removed FROM installed WHERE id=?`, id).Scan(&revision, &removed); err != nil {
			return Configuration{}, err
		}
		if removed != 0 || revision != item.Revision {
			return Configuration{}, ErrRevision
		}
		var unfinished, migrating int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE plugin=? AND state NOT IN ('completed','failed','cancelled'))`, id).Scan(&unfinished); err != nil {
			return Configuration{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM migrations WHERE plugin=? AND state='pending')`, id).Scan(&migrating); err != nil {
			return Configuration{}, err
		}
		if unfinished != 0 || migrating != 0 {
			return Configuration{}, errors.New("plugin configuration has unfinished work")
		}
	}
	revision := 0
	private := map[string]string{}
	var old []byte
	err = tx.QueryRowContext(ctx, `SELECT revision,secrets_json FROM configurations WHERE plugin=?`, id).Scan(&revision, &old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Configuration{}, err
	}
	if len(old) > 0 && json.Unmarshal(old, &private) != nil {
		return Configuration{}, errors.New("invalid private configuration")
	}
	if revision != input.Revision {
		return Configuration{}, ErrRevision
	}
	for name, value := range input.Secrets {
		field, ok := schema.Properties[name]
		if !ok || !field.Secret {
			return Configuration{}, errors.New("secret field not declared")
		}
		if value != "" {
			raw, _ := json.Marshal(value)
			if err = validateField(field, raw); err != nil {
				return Configuration{}, errors.New("invalid secret value")
			}
			private[name] = value
		}
	}
	for _, name := range input.ClearSecrets {
		field, ok := schema.Properties[name]
		if !ok || !field.Secret {
			return Configuration{}, errors.New("clear-secret field not declared")
		}
		if input.Secrets[name] != "" {
			return Configuration{}, errors.New("cannot set and clear same secret")
		}
		delete(private, name)
	}
	for _, name := range schema.Required {
		field := schema.Properties[name]
		if field.Secret {
			if private[name] == "" {
				return Configuration{}, fmt.Errorf("required secret not configured: %s", name)
			}
		} else if _, ok := values[name]; !ok {
			return Configuration{}, fmt.Errorf("required configuration missing: %s", name)
		}
	}
	for name := range private {
		if field, ok := schema.Properties[name]; !ok || !field.Secret {
			delete(private, name)
		}
	}
	if err := validateConfiguredRoutes(item.Package, values); err != nil {
		return Configuration{}, err
	}
	rawValues, _ := json.Marshal(values)
	rawSecrets, _ := json.Marshal(private)
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM configuration_versions WHERE plugin=?`, id).Scan(&revision); err != nil {
		return Configuration{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO configurations(plugin,revision,values_json,secrets_json) VALUES(?,?,?,?) ON CONFLICT(plugin) DO UPDATE SET revision=excluded.revision,values_json=excluded.values_json,secrets_json=excluded.secrets_json`, id, revision, rawValues, rawSecrets)
	if err != nil {
		return Configuration{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_versions(plugin,revision,values_json,secrets_json) VALUES(?,?,?,?)`, id, revision, rawValues, rawSecrets); err != nil {
		return Configuration{}, err
	}
	if onCommit != nil {
		if err = onCommit(tx, revision); err != nil {
			return Configuration{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Configuration{}, err
	}
	return s.Configuration(ctx, id)
}
