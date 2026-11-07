//go:build windows

package nativeclient

import (
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func syntheticExecutable(t *testing.T) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// Windows resource rewriting does not relocate Go's optional COFF symbol
	// table. Remove its header references in this nonexecuted synthetic copy.
	offset := int(binary.LittleEndian.Uint32(raw[0x3c:]))
	clear(raw[offset+12 : offset+20])
	path := filepath.Join(t.TempDir(), "synthetic-client.exe")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectByteIdentityAndMissingMetadata(t *testing.T) {
	path := syntheticExecutable(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(t.Context(), path)
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("missing version error = %v", err)
	}
	sum := sha256.Sum256(raw)
	if info.SHA256 != hex.EncodeToString(sum[:]) || info.Bytes != int64(len(raw)) || info.Architecture != "x64" {
		t.Fatalf("identity = %+v", info)
	}
	encoded, _ := json.Marshal(info)
	if strings.Contains(string(encoded), path) || strings.Contains(string(encoded), "path") {
		t.Fatal("local path escaped into metadata")
	}
}

func TestInspectSyntheticVersionResource(t *testing.T) {
	path := syntheticExecutable(t)
	addVersionResource(t, path, syntheticVersion("Synthetic Test Client", "Synthetic Test Publisher", "synthetic-client.exe"))
	if p, err := pe.Open(path); err != nil {
		t.Fatalf("synthetic PE: %v", err)
	} else {
		p.Close()
	}
	info, err := Inspect(t.Context(), path)
	if err != nil {
		t.Fatalf("inspect synthetic version: %v", err)
	}
	if info.Version != "1.2.3.4" || info.ProductName != "Synthetic Test Client" || info.CompanyName != "Synthetic Test Publisher" || info.OriginalFilename != "synthetic-client.exe" {
		t.Fatalf("version = %+v", info)
	}
}

func TestInspectRejectsPathsAndUnsupportedImages(t *testing.T) {
	for _, path := range []string{"relative.exe", `\\server\share\client.exe`, `\\?\C:\client.exe`, `C:\client.exe:alternate.exe`, `C:\folder\..\client.exe`, "C:\\bad\x00.exe", `C:\client.dll`} {
		if _, err := Inspect(t.Context(), path); !errors.Is(err, ErrInput) {
			t.Errorf("input rejected with %v", err)
		}
	}
	for _, mode := range []string{"malformed", "x86", "dll", "not-executable"} {
		t.Run(mode, func(t *testing.T) {
			path := syntheticExecutable(t)
			raw, _ := os.ReadFile(path)
			offset := int(binary.LittleEndian.Uint32(raw[0x3c:]))
			switch mode {
			case "malformed":
				raw = []byte("synthetic invalid PE")
			case "x86":
				binary.LittleEndian.PutUint16(raw[offset+4:], 0x14c)
			case "dll":
				binary.LittleEndian.PutUint16(raw[offset+22:], binary.LittleEndian.Uint16(raw[offset+22:])|0x2000)
			case "not-executable":
				binary.LittleEndian.PutUint16(raw[offset+22:], binary.LittleEndian.Uint16(raw[offset+22:])&^2)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(t.Context(), path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("image error = %v", err)
			}
		})
	}
	dir := filepath.Join(t.TempDir(), "directory.exe")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), dir); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestInspectRejectsFileAndAncestorLinks(t *testing.T) {
	path := syntheticExecutable(t)
	link := filepath.Join(t.TempDir(), "linked.exe")
	if err := os.Symlink(path, link); err != nil {
		t.Skip("Windows symlink privilege unavailable")
	}
	if _, err := Inspect(t.Context(), link); err == nil {
		t.Fatal("file symlink accepted")
	}
	parent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(filepath.Dir(path), parent); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), filepath.Join(parent, filepath.Base(path))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ancestor link error = %v", err)
	}
}

func TestInspectRejectsAncestorJunction(t *testing.T) {
	path := syntheticExecutable(t)
	link := filepath.Join(t.TempDir(), "junction")
	if err := os.Mkdir(link, 0700); err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(link)
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	substitute := versionUTF16(`\??\` + filepath.Dir(path))
	printName := versionUTF16(filepath.Dir(path))
	raw := make([]byte, 16)
	raw = append(raw, substitute...)
	raw = append(raw, printName...)
	binary.LittleEndian.PutUint32(raw, windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(raw[4:], uint16(len(raw)-8))
	binary.LittleEndian.PutUint16(raw[10:], uint16(len(substitute)-2))
	binary.LittleEndian.PutUint16(raw[12:], uint16(len(substitute)))
	binary.LittleEndian.PutUint16(raw[14:], uint16(len(printName)-2))
	var returned uint32
	err = windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &raw[0], uint32(len(raw)), nil, 0, &returned, nil)
	windows.CloseHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(t.Context(), filepath.Join(link, filepath.Base(path))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("junction error = %v", err)
	}
}

type checkingContext struct {
	context.Context
	calls    int
	check    func()
	cancelAt int
}

func (c *checkingContext) Err() error {
	c.calls++
	if c.calls == 3 && c.check != nil {
		c.check()
	}
	if c.cancelAt > 0 && c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestInspectPinsFileAndAncestorsAndCancelsHash(t *testing.T) {
	path := syntheticExecutable(t)
	ctx := &checkingContext{Context: context.Background(), check: func() {
		if err := os.WriteFile(path, []byte("must not replace"), 0600); err == nil {
			t.Error("write allowed during inspection")
		}
		if err := os.Rename(path, path+".moved"); err == nil {
			t.Error("file replacement allowed during inspection")
		}
		if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-moved"); err == nil {
			t.Error("parent replacement allowed during inspection")
		}
	}}
	if _, err := Inspect(ctx, path); !errors.Is(err, ErrVersion) {
		t.Fatal(err)
	}
	ctx = &checkingContext{Context: context.Background(), cancelAt: 4}
	if info, err := Inspect(ctx, path); !errors.Is(err, context.Canceled) || info.SHA256 != "" {
		t.Fatalf("hash cancellation = %+v, %v", info, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Inspect(canceled, path); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInspectRejectsIncompleteAndOversizedVersionStrings(t *testing.T) {
	for _, product := range []string{"", strings.Repeat("x", 514), "bad\nproduct"} {
		path := syntheticExecutable(t)
		addVersionResource(t, path, syntheticVersion(product, "Synthetic", "synthetic.exe"))
		if info, err := Inspect(t.Context(), path); !errors.Is(err, ErrVersion) || info.Version != "1.2.3.4" {
			t.Fatalf("invalid metadata = %+v, %v", info, err)
		}
	}
}

// Fixtures use only a copy of this Go test executable, never a third-party
// binary. UpdateResource writes synthetic declared metadata; no sample runs.
func addVersionResource(t *testing.T, path string, resource []byte) {
	t.Helper()
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	begin := kernel.NewProc("BeginUpdateResourceW")
	update := kernel.NewProc("UpdateResourceW")
	end := kernel.NewProc("EndUpdateResourceW")
	name, _ := windows.UTF16PtrFromString(path)
	handle, _, err := begin.Call(uintptr(unsafe.Pointer(name)), 0)
	if handle == 0 {
		t.Fatalf("begin resource: %v", err)
	}
	result, _, err := update.Call(handle, 16, 1, 0x0409, uintptr(unsafe.Pointer(&resource[0])), uintptr(len(resource)))
	if result == 0 {
		_, _, _ = end.Call(handle, 1)
		t.Fatalf("update resource: %v", err)
	}
	result, _, err = end.Call(handle, 0)
	if result == 0 {
		t.Fatalf("finish resource: %v", err)
	}
}

func versionUTF16(value string) []byte {
	units := utf16.Encode([]rune(value + "\x00"))
	raw := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(raw[i*2:], unit)
	}
	return raw
}
func versionBlock(key string, kind uint16, value []byte, children ...[]byte) []byte {
	raw := make([]byte, 6)
	raw = append(raw, versionUTF16(key)...)
	for len(raw)%4 != 0 {
		raw = append(raw, 0)
	}
	raw = append(raw, value...)
	for _, child := range children {
		for len(raw)%4 != 0 {
			raw = append(raw, 0)
		}
		raw = append(raw, child...)
	}
	binary.LittleEndian.PutUint16(raw, uint16(len(raw)))
	length := len(value)
	if kind == 1 {
		length /= 2
	}
	binary.LittleEndian.PutUint16(raw[2:], uint16(length))
	binary.LittleEndian.PutUint16(raw[4:], kind)
	return raw
}
func syntheticVersion(product, company, original string) []byte {
	fixed := make([]byte, 52)
	for index, value := range []uint32{0xfeef04bd, 0x10000, 0x10002, 0x30004, 0x10002, 0x30004, 0x3f, 0, 0x40004, 1} {
		binary.LittleEndian.PutUint32(fixed[index*4:], value)
	}
	strings := versionBlock("StringFileInfo", 1, nil, versionBlock("040904b0", 1, nil,
		versionBlock("ProductName", 1, versionUTF16(product)), versionBlock("CompanyName", 1, versionUTF16(company)), versionBlock("OriginalFilename", 1, versionUTF16(original))))
	translation := []byte{0x09, 0x04, 0xb0, 0x04}
	vars := versionBlock("VarFileInfo", 1, nil, versionBlock("Translation", 0, translation))
	return versionBlock("VS_VERSION_INFO", 0, fixed, strings, vars)
}
