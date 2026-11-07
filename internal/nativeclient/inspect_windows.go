//go:build windows

// Package nativeclient inspects owner-selected local executables without
// executing them. Version strings are file declarations, not proof of origin.
package nativeclient

import (
	"context"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ErrInput       = errors.New("native client executable path is invalid")
	ErrInvalid     = errors.New("native client is not a regular Windows x64 executable")
	ErrVersion     = errors.New("native client version metadata is missing or invalid")
	ErrUnavailable = errors.New("native client executable cannot be read")
)

// Info contains no local path. Metadata and a digest identify the inspected
// bytes; they do not authenticate a publisher or imply a supported control API.
type Info struct {
	SHA256           string `json:"sha256"`
	Bytes            int64  `json:"bytes"`
	Architecture     string `json:"architecture"`
	Version          string `json:"version"`
	ProductName      string `json:"product_name"`
	CompanyName      string `json:"company_name"`
	OriginalFilename string `json:"original_filename"`
}

// Inspect keeps the executable and every ancestor pinned against replacement
// while hashing, parsing its PE headers and reading Windows version resources.
// Missing metadata returns the available byte identity together with ErrVersion.
func Inspect(ctx context.Context, path string) (Info, error) {
	var info Info
	if err := ctx.Err(); err != nil {
		return info, err
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.EqualFold(filepath.Ext(path), ".exe") || strings.ContainsAny(path[2:], ":\x00") {
		return info, ErrInput
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil || windows.GetDriveType(root) == windows.DRIVE_REMOTE {
		return info, ErrInput
	}
	dirs, err := pinDirectories(filepath.Dir(path))
	if err != nil {
		return info, err
	}
	defer func() {
		for i := len(dirs) - 1; i >= 0; i-- {
			_ = windows.CloseHandle(dirs[i])
		}
	}()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return info, ErrInput
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return info, ErrUnavailable
	}
	f := os.NewFile(uintptr(h), "native-client.exe")
	defer f.Close()
	var attrs windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &attrs) != nil {
		return info, ErrUnavailable
	}
	if attrs.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return info, ErrInvalid
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return info, ErrInvalid
	}
	parsed, err := pe.NewFile(f)
	if err != nil {
		return info, ErrInvalid
	}
	defer parsed.Close()
	if parsed.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || parsed.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || parsed.Characteristics&pe.IMAGE_FILE_DLL != 0 {
		return info, ErrInvalid
	}
	if _, ok := parsed.OptionalHeader.(*pe.OptionalHeader64); !ok {
		return info, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return info, err
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return Info{}, err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Info{}, ErrUnavailable
		}
	}
	info.SHA256, info.Bytes, info.Architecture = hex.EncodeToString(hash.Sum(nil)), stat.Size(), "x64"
	versionErr := inspectVersion(path, &info)
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	return info, versionErr
}

func pinDirectories(path string) ([]windows.Handle, error) {
	var handles []windows.Handle
	closeAll := func() {
		for _, h := range handles {
			_ = windows.CloseHandle(h)
		}
	}
	current := filepath.VolumeName(path) + `\`
	parts := []string{current}
	for _, part := range strings.Split(strings.TrimPrefix(path, current), `\`) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		parts = append(parts, current)
	}
	for _, part := range parts {
		name, err := windows.UTF16PtrFromString(part)
		if err != nil {
			closeAll()
			return nil, ErrInput
		}
		h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			closeAll()
			return nil, ErrUnavailable
		}
		handles = append(handles, h)
		var attrs windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(h, &attrs) != nil {
			closeAll()
			return nil, ErrUnavailable
		}
		if attrs.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attrs.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			closeAll()
			return nil, ErrInvalid
		}
	}
	return handles, nil
}

const maxVersionBytes = 1 << 20

var (
	versionDLL  = windows.NewLazySystemDLL("version.dll")
	versionSize = versionDLL.NewProc("GetFileVersionInfoSizeExW")
	versionRead = versionDLL.NewProc("GetFileVersionInfoExW")
)

func inspectVersion(path string, info *Info) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ErrVersion
	}
	if versionSize.Find() != nil || versionRead.Find() != nil {
		return ErrVersion
	}
	var ignored uint32
	// FILE_VER_GET_NEUTRAL reads this binary's resource, without merging an
	// unpinned external language-specific MUI resource into its byte identity.
	const neutral = 2
	size, _, _ := versionSize.Call(neutral, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&ignored)))
	if size == 0 || size > maxVersionBytes {
		return ErrVersion
	}
	buffer := make([]byte, int(size))
	read, _, _ := versionRead.Call(neutral, uintptr(unsafe.Pointer(name)), 0, size, uintptr(unsafe.Pointer(&buffer[0])))
	if read == 0 {
		return ErrVersion
	}
	query := func(key string, unit uintptr) (unsafe.Pointer, uint32, bool) {
		var pointer unsafe.Pointer
		var count uint32
		if windows.VerQueryValue(unsafe.Pointer(&buffer[0]), key, unsafe.Pointer(&pointer), &count) != nil || pointer == nil || count == 0 {
			return nil, 0, false
		}
		base, start := uintptr(unsafe.Pointer(&buffer[0])), uintptr(pointer)
		length := uintptr(count) * unit
		if start < base || start-base >= uintptr(len(buffer)) || length > uintptr(len(buffer))-(start-base) {
			return nil, 0, false
		}
		return pointer, count, true
	}
	pointer, count, ok := query(`\`, 1)
	if !ok || uintptr(count) < unsafe.Sizeof(windows.VS_FIXEDFILEINFO{}) {
		return ErrVersion
	}
	fixed := *(*windows.VS_FIXEDFILEINFO)(pointer)
	if fixed.Signature != 0xfeef04bd {
		return ErrVersion
	}
	info.Version = fmt.Sprintf("%d.%d.%d.%d", fixed.FileVersionMS>>16, fixed.FileVersionMS&0xffff, fixed.FileVersionLS>>16, fixed.FileVersionLS&0xffff)
	pointer, count, ok = query(`\VarFileInfo\Translation`, 1)
	if !ok || count%4 != 0 || count > 1024 {
		return ErrVersion
	}
	translations := unsafe.Slice((*uint16)(pointer), int(count)/2)
	for i := 0; i+1 < len(translations); i += 2 {
		values := make([]string, 3)
		for j, field := range []string{"ProductName", "CompanyName", "OriginalFilename"} {
			key := fmt.Sprintf(`\StringFileInfo\%04x%04x\%s`, translations[i], translations[i+1], field)
			p, n, found := query(key, 2)
			if !found || n > 513 {
				continue
			}
			chars := unsafe.Slice((*uint16)(p), int(n))
			if chars[len(chars)-1] != 0 {
				continue
			}
			if strings.ContainsRune(string(utf16.Decode(chars[:len(chars)-1])), 0) {
				continue
			}
			value := strings.TrimSpace(windows.UTF16ToString(chars))
			if value != "" && !strings.ContainsFunc(value, unicode.IsControl) {
				values[j] = value
			}
		}
		if values[0] != "" && values[1] != "" && values[2] != "" {
			info.ProductName, info.CompanyName, info.OriginalFilename = values[0], values[1], values[2]
			return nil
		}
	}
	return ErrVersion
}
