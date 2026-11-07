// Package pluginhost owns package contracts independently of service providers.
package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const Protocol = 1

var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Package is distinct from the legacy HTTP tool manifest. A descriptor is not
// permission to install, execute or send a message.
type Package struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	HostProtocol  int               `json:"host_protocol"`
	Status        string            `json:"status"`
	Platforms     []string          `json:"platforms"`
	Runtime       Runtime           `json:"runtime"`
	Permissions   []string          `json:"permissions"`
	Dependencies  Dependencies      `json:"dependencies"`
	HTTPRoutes    []HTTPRoute       `json:"http_routes,omitempty"`
	Files         map[string]string `json:"files"`
}

// Dependency is an offline, package-declared compatibility requirement. Version
// accepts an exact semantic version or a space-separated set of <, <=, >, >=,
// ^ and ~ comparators. A missing Version accepts any valid installed version
// for legacy descriptors, but newly authored packages should constrain it.
type Dependency struct {
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
}

// Dependencies accepts legacy ["id"] manifests as well as the fixed object
// form [{"id":"id","version":"^1.2.0"}]. The canonical serialized form
// is always objects once a package has been prepared and installed.
type Dependencies []Dependency

func (d *Dependencies) UnmarshalJSON(raw []byte) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	items := make(Dependencies, len(entries))
	objectForm := false
	for i, entry := range entries {
		entry = json.RawMessage(strings.TrimSpace(string(entry)))
		if len(entry) == 0 {
			return errors.New("invalid dependency declaration")
		}
		if entry[0] == '"' {
			if i > 0 && objectForm {
				return errors.New("mixed dependency declaration")
			}
			if err := json.Unmarshal(entry, &items[i].ID); err != nil {
				return err
			}
			continue
		}
		if i > 0 && !objectForm {
			return errors.New("mixed dependency declaration")
		}
		objectForm = true
		decoder := json.NewDecoder(strings.NewReader(string(entry)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&items[i]); err != nil {
			return errors.New("invalid dependency declaration")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return errors.New("invalid dependency declaration")
		}
	}
	*d = items
	return nil
}

type semanticVersion struct{ major, minor, patch int }

func parseSemanticVersion(raw string) (semanticVersion, bool) {
	var v semanticVersion
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return v, false
	}
	fields := []*int{&v.major, &v.minor, &v.patch}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return v, false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return v, false
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 1_000_000 {
			return v, false
		}
		*fields[i] = n
	}
	return v, true
}

func compareSemanticVersion(a, b semanticVersion) int {
	for _, pair := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

func validDependencyConstraint(raw string) bool {
	if raw == "" {
		return true
	}
	if strings.TrimSpace(raw) == "" {
		return false
	}
	for _, term := range strings.Fields(raw) {
		operator, version := "", term
		for _, prefix := range []string{">=", "<=", "^", "~", ">", "<", "="} {
			if strings.HasPrefix(term, prefix) {
				operator, version = prefix, strings.TrimPrefix(term, prefix)
				break
			}
		}
		if _, ok := parseSemanticVersion(version); !ok || operator == "=" && strings.Count(term, "=") != 1 {
			return false
		}
	}
	return true
}

// DependencyCompatible evaluates only declared semantic versions. It never
// installs, enables or selects a replacement package.
func DependencyCompatible(requirement, installed string) bool {
	if requirement == "" {
		// Legacy descriptors had no version field and may use an opaque package
		// version such as "1". Treating those as a new semantic constraint
		// would make an otherwise valid installed graph unusable.
		return installed != ""
	}
	actual, ok := parseSemanticVersion(installed)
	if !ok || !validDependencyConstraint(requirement) {
		return false
	}
	for _, term := range strings.Fields(requirement) {
		op, value := "=", term
		for _, prefix := range []string{">=", "<=", "^", "~", ">", "<", "="} {
			if strings.HasPrefix(term, prefix) {
				op, value = prefix, strings.TrimPrefix(term, prefix)
				break
			}
		}
		want, _ := parseSemanticVersion(value)
		cmp := compareSemanticVersion(actual, want)
		caret := cmp >= 0 && (want.major > 0 && actual.major == want.major || want.major == 0 && want.minor > 0 && actual.major == 0 && actual.minor == want.minor || want.major == 0 && want.minor == 0 && actual.major == 0 && actual.minor == 0 && actual.patch == want.patch)
		match := map[string]bool{"=": cmp == 0, ">": cmp > 0, ">=": cmp >= 0, "<": cmp < 0, "<=": cmp <= 0, "^": caret, "~": cmp >= 0 && actual.major == want.major && actual.minor == want.minor}[op]
		if !match {
			return false
		}
	}
	return true
}

type Runtime struct {
	Transport  string `json:"transport"`
	Entrypoint string `json:"entrypoint,omitempty"`
}

func safePath(p string) bool {
	return p != "" && p != "." && fs.ValidPath(p) && path.Clean(p) == p && !strings.ContainsAny(p, "\\:")
}

// Verify only reads a package. Every regular file except the manifest must be
// covered by its digest; links, special files and unexpected files are rejected.
// Digests prove integrity, not publisher trust. The installer must separately
// bind the verified package to an administrator-approved source.
func Verify(dir string) (Package, error) {
	return VerifyDescriptor(dir, "plugin.json")
}

// VerifyDescriptor lets connected applications share the package integrity
// contract. The descriptor name does not grant installation or execution.
func VerifyDescriptor(dir, descriptor string) (Package, error) {
	var p Package
	if descriptor != "plugin.json" && descriptor != "app.json" {
		return p, errors.New("unsupported package descriptor")
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return p, errors.New("package directory unavailable")
	}
	manifest := filepath.Join(dir, descriptor)
	info, err := os.Lstat(manifest)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return p, errors.New("invalid manifest file")
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return p, err
	}
	if len(raw) > 1<<20 {
		return p, errors.New("manifest exceeds 1 MiB")
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, errors.New("invalid manifest")
	}
	if p.SchemaVersion != 1 || p.HostProtocol != Protocol || !identifier.MatchString(p.ID) || strings.TrimSpace(p.Name) == "" || p.Version == "" {
		return p, errors.New("unsupported package identity or protocol")
	}
	switch p.Status {
	case "template", "migration-pending", "ready":
	default:
		return p, errors.New("invalid implementation status")
	}
	switch p.Runtime.Transport {
	case "mcp-stdio", "mcp-http", "http-adapter", "unbound":
	default:
		return p, errors.New("invalid transport")
	}
	if p.Status == "ready" && p.Runtime.Transport == "unbound" {
		return p, errors.New("ready package requires runtime")
	}
	if p.Runtime.Entrypoint != "" && !safePath(p.Runtime.Entrypoint) {
		return p, errors.New("unsafe entrypoint")
	}
	if len(p.Platforms) == 0 {
		return p, errors.New("platforms missing")
	}
	seen := map[string]bool{}
	for _, permission := range p.Permissions {
		if !allowedPermission(permission) || seen[permission] {
			return p, errors.New("unknown or repeated permission")
		}
		seen[permission] = true
	}
	seenDependencies := map[string]bool{}
	for _, dependency := range p.Dependencies {
		if !identifier.MatchString(dependency.ID) || dependency.ID == p.ID || seenDependencies[dependency.ID] || !validDependencyConstraint(dependency.Version) {
			return p, errors.New("invalid dependency")
		}
		seenDependencies[dependency.ID] = true
	}
	if err := validateHTTPRoutes(p); err != nil {
		return p, err
	}
	for name, digest := range p.Files {
		if !safePath(name) || name == descriptor || len(digest) != 64 {
			return p, errors.New("invalid file index")
		}
		if _, err = hex.DecodeString(digest); err != nil {
			return p, errors.New("invalid digest")
		}
	}
	for _, name := range []string{"config.schema.json", "ui.schema.json", "capabilities.json", "README.md", "PERMISSIONS.md", "MIGRATION.md", "CHANGELOG.md"} {
		if _, ok := p.Files[name]; !ok {
			return p, fmt.Errorf("required file missing: %s", name)
		}
	}
	found := map[string]bool{}
	err = filepath.WalkDir(dir, func(full string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("package links forbidden")
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("special file forbidden")
		}
		rel, e := filepath.Rel(dir, full)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if rel == descriptor {
			return nil
		}
		expected, ok := p.Files[rel]
		if !ok {
			return fmt.Errorf("unindexed package file: %s", rel)
		}
		f, e := os.Open(full)
		if e != nil {
			return e
		}
		hash := sha256.New()
		_, e = io.Copy(hash, f)
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != expected {
			return fmt.Errorf("package digest mismatch: %s", rel)
		}
		found[rel] = true
		return nil
	})
	if err != nil {
		return p, err
	}
	if len(found) != len(p.Files) {
		return p, errors.New("indexed file missing")
	}
	if p.Runtime.Entrypoint != "" && !found[p.Runtime.Entrypoint] {
		return p, errors.New("entrypoint missing")
	}
	if err := validateHTTPRouteResources(dir, p); err != nil {
		return p, err
	}
	return p, nil
}
func allowedPermission(p string) bool {
	switch p {
	case "network", "gpu", "device", "workspace.read", "workspace.write", "process.execute", "artifact.read", "artifact.write", "model.invoke":
		return true
	}
	return false
}
