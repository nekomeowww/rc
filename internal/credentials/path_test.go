package credentials

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMountPath(t *testing.T) {
	t.Parallel()

	for name, mountPath := range map[string]string{
		"POSIX":                 testProcessCredentialMountPath,
		"WindowsBackslashes":    `C:\home\agent\.tool\credentials.json`,
		"WindowsForwardSlashes": "C:/home/agent/.tool/credentials.json",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, ValidateMountPath(mountPath))
		})
	}
}

func TestValidateMountPathRejectsNonFilePaths(t *testing.T) {
	t.Parallel()

	for name, mountPath := range map[string]string{
		"Empty":                "",
		"Relative":             "credentials.json",
		"POSIXRoot":            "/",
		"POSIXParent":          "/home/agent/../credentials.json",
		"WindowsDriveRelative": `C:credentials.json`,
		"WindowsRoot":          `C:\`,
		"WindowsParent":        `C:\home\agent\..\credentials.json`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Error(t, ValidateMountPath(mountPath))
		})
	}
}
