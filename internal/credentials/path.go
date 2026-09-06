package credentials

import (
	"fmt"
	"path"
	"strings"
)

// ValidateMountPath accepts clean POSIX and Windows drive-absolute file paths
// regardless of the operating system on which rcctl or the controller runs.
func ValidateMountPath(value string) error {
	if isCleanAbsoluteMountPath(value) {
		return nil
	}
	return fmt.Errorf("credential mount path %q must be a clean absolute file path", value)
}

func isCleanAbsoluteMountPath(value string) bool {
	if strings.HasPrefix(value, "/") {
		return value != "/" && path.Clean(value) == value
	}
	if len(value) < 3 || !isASCIILetter(value[0]) || value[1] != ':' || value[2] != '/' && value[2] != '\\' {
		return false
	}

	normalized := strings.ReplaceAll(value, `\`, "/")
	return path.Clean(normalized) == normalized
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
