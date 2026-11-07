package pluginruntime

import "strings"

// WindowsProcessPath formats an already validated Windows resource path for
// subprocesses which otherwise hit MAX_PATH. It does not grant file access.
func WindowsProcessPath(path string) string {
	if path == "" || strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
	}
	return `\\?\` + path
}
