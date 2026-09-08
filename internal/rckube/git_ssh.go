package rckube

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configureWindowsGitSSH gives Git's bundled OpenSSH a process-owned config.
// MSYS OpenSSH does not discover the native Windows home reliably and treats
// drive-qualified Include paths as relative paths. Flatten the selected rc
// fragments while keeping the ordinary home config available to native SSH.
func configureWindowsGitSSH(environment map[string]string, runtimeDirectory string, fragments []string) error {
	if len(fragments) == 0 {
		return nil
	}
	for name := range environment {
		if strings.EqualFold(name, "GIT_SSH_COMMAND") || strings.EqualFold(name, "GIT_SSH") {
			return nil
		}
	}
	if _, exists := os.LookupEnv("GIT_SSH_COMMAND"); exists {
		return nil
	}
	if _, exists := os.LookupEnv("GIT_SSH"); exists {
		return nil
	}
	var config strings.Builder
	for _, fragment := range fragments {
		data, err := os.ReadFile(fragment)
		if err != nil {
			return fmt.Errorf("read Git SSH credential configuration: %w", err)
		}
		config.WriteString("Host *\n")
		config.Write(data)
		config.WriteByte('\n')
	}
	configPath := filepath.Join(runtimeDirectory, "git-ssh.conf")
	if err := os.WriteFile(configPath, []byte(config.String()), 0o600); err != nil {
		return fmt.Errorf("write Git SSH credential configuration: %w", err)
	}
	// Git interprets GIT_SSH_COMMAND with a shell, including on Windows.
	quotedPath := "'" + strings.ReplaceAll(filepath.ToSlash(configPath), "'", "'\"'\"'") + "'"
	environment["GIT_SSH_COMMAND"] = "ssh -F " + quotedPath
	return nil
}
