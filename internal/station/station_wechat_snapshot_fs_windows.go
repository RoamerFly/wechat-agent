//go:build windows

package station

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"wechat-bot/internal/workspacefs"
)

// createLegacySnapshotStage creates one new, final staging leaf with an
// explicit protected DACL. It intentionally never repairs or adopts an
// existing leaf: a pre-existing name is an integrity failure for a snapshot.
func createLegacySnapshotStage(parent *os.Root, name string) error {
	if err := validLegacySnapshotLeaf(name); err != nil {
		return err
	}
	parentPath, err := legacySnapshotLocalPath(parent)
	if err != nil {
		return err
	}
	user, err := legacySnapshotCurrentUserSID()
	if err != nil {
		return err
	}
	descriptor, err := legacySnapshotPrivateDescriptor(user)
	if err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(parentPath, name))
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	if err := windows.CreateDirectory(path, &attributes); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return fmt.Errorf("legacy snapshot staging leaf already exists: %w", err)
		}
		return err
	}
	stage, err := workspacefs.OpenExistingDirectory(parent, name)
	if err != nil {
		return fmt.Errorf("open newly-created legacy snapshot staging leaf: %w", err)
	}
	defer stage.Close()
	return verifyLegacySnapshotPrivacy(stage)
}

// verifyLegacySnapshotPrivacy verifies the ACL through a handle opened from
// the supplied root, rather than by resolving a caller-supplied path again.
func verifyLegacySnapshotPrivacy(root *os.Root) error {
	if root == nil {
		return errors.New("legacy snapshot root is required")
	}
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("legacy snapshot root is not a directory")
	}
	user, err := legacySnapshotCurrentUserSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return errors.New("legacy snapshot directory owner mismatch")
	}
	sddl := descriptor.String()
	ownerACE := "(A;OICI;FA;;;" + user.String() + ")"
	systemACE := "(A;OICI;FA;;;SY)"
	if !strings.Contains(sddl, "D:P") || !strings.Contains(sddl, ownerACE) || !strings.Contains(sddl, systemACE) || strings.Count(sddl, "(") != 2 {
		return errors.New("legacy snapshot directory dacl mismatch")
	}
	return nil
}

// publishLegacySnapshot atomically changes the staged leaf name without ever
// replacing an existing entry. os.Root.Rename is intentionally not used: on
// Windows it delegates to os.Rename, which calls MoveFileEx with
// MOVEFILE_REPLACE_EXISTING. A preflight Lstat plus that operation could race
// and replace a concurrently-created file. MoveFileEx with zero flags instead
// fails when the destination is present. Both names are single validated
// leaves below the opened parent; its final path is derived from a live handle.
func publishLegacySnapshot(parent *os.Root, temp, name string) error {
	if err := validLegacySnapshotLeaf(temp); err != nil {
		return err
	}
	if err := validLegacySnapshotLeaf(name); err != nil {
		return err
	}
	if temp == name {
		return errors.New("legacy snapshot staging and destination leaves must differ")
	}
	stage, err := workspacefs.OpenExistingDirectory(parent, temp)
	if err != nil {
		return fmt.Errorf("open legacy snapshot staging leaf: %w", err)
	}
	stageInfo, err := legacySnapshotDirectoryInfo(stage)
	if err != nil {
		stage.Close()
		return err
	}
	if err := verifyLegacySnapshotPrivacy(stage); err != nil {
		stage.Close()
		return err
	}
	stage.Close()
	if _, err := parent.Lstat(name); err == nil {
		return errors.New("legacy snapshot destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check legacy snapshot destination: %w", err)
	}
	parentPath, err := legacySnapshotLocalPath(parent)
	if err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(filepath.Join(parentPath, temp))
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(filepath.Join(parentPath, name))
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, 0); err != nil {
		return err
	}
	published, err := workspacefs.OpenExistingDirectory(parent, name)
	if err != nil {
		return fmt.Errorf("open published legacy snapshot: %w", err)
	}
	defer published.Close()
	publishedInfo, err := legacySnapshotDirectoryInfo(published)
	if err != nil {
		return err
	}
	if !os.SameFile(stageInfo, publishedInfo) {
		return errors.New("published legacy snapshot identity changed")
	}
	return verifyLegacySnapshotPrivacy(published)
}

func validLegacySnapshotLeaf(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || filepath.Clean(name) != name ||
		strings.ContainsAny(name, `\\/:"<>|?*`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") || legacySnapshotReservedLeaf(name) {
		return errors.New("legacy snapshot path must be one relative leaf")
	}
	return nil
}

func legacySnapshotReservedLeaf(name string) bool {
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func legacySnapshotCurrentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func legacySnapshotPrivateDescriptor(user *windows.SID) (*windows.SECURITY_DESCRIPTOR, error) {
	// D:P prevents parent ACL inheritance. OICI makes the sole owner and
	// SYSTEM full-control entries inherit to the snapshot's children.
	return windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;FA;;;SY)")
}

// legacySnapshotLocalPath resolves the root from a live handle, normalizes its
// Win32 long-path form, and permits only local drive-letter volumes. Network
// paths, mapped drives, and UNC shares are deliberately not snapshot targets:
// the protected local ACL contract cannot make their remote server semantics
// equivalent to a local private directory.
func legacySnapshotLocalPath(root *os.Root) (string, error) {
	if root == nil {
		return "", errors.New("legacy snapshot parent root is required")
	}
	file, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("legacy snapshot parent root is not a directory")
	}
	// A final path can be up to 32K UTF-16 code units with the Win32 long-path
	// prefix. Failing instead of truncating keeps this security boundary closed.
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", err
	}
	if n == 0 || n >= uint32(len(buffer)) {
		return "", errors.New("legacy snapshot parent path is too long")
	}
	path := windows.UTF16ToString(buffer[:n])
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return "", errors.New("legacy snapshot parent must be on a local drive")
	}
	path = strings.TrimPrefix(path, `\\?\`)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("legacy snapshot parent path is not a clean absolute path")
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return "", errors.New("legacy snapshot parent must use a drive-letter volume")
	}
	driveRoot, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return "", err
	}
	switch windows.GetDriveType(driveRoot) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_RAMDISK:
		return path, nil
	default:
		return "", errors.New("legacy snapshot parent drive is not local")
	}
}

func legacySnapshotDirectoryInfo(root *os.Root) (os.FileInfo, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("legacy snapshot root is not a directory")
	}
	return info, nil
}
