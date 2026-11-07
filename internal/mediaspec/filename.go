package mediaspec

import "strings"

// SafeFilename checks a display filename for separators, NUL, and line breaks.
// Callers still enforce the destination filesystem and its reserved names.
func SafeFilename(name string) bool {
	return name != "" && len(name) <= 512 && !strings.ContainsAny(name, "/\\\x00\r\n")
}
