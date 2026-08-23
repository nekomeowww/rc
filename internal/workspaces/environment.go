/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workspaces

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var excludedCallerEnvironment = map[string]struct{}{
	"PATH": {}, "HOME": {}, "PWD": {}, "OLDPWD": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	"SHLVL": {}, "_": {}, "KUBECONFIG": {}, "XDG_RUNTIME_DIR": {}, "SSH_AUTH_SOCK": {},
	"TERM": {}, "COLORTERM": {}, "LINES": {}, "COLUMNS": {}, "CODEX_HOME": {},
	"TMP": {}, "TEMP": {}, "TMPDIR": {}, "COMMAND_MODE": {}, "GIT_ASKPASS": {},
	"NODE_OPTIONS": {}, "PYTHONSTARTUP": {}, "PYTHON_BASIC_REPL": {}, "TERMINFO": {},
	"TERMINFO_DIRS": {}, "USER_ZDOTDIR": {}, "ZDOTDIR": {}, "MallocNanoZone": {},
	"OSLogRateLimit": {},
	"CDPATH":         {}, "CLASSPATH": {}, "FPATH": {}, "GOPATH": {}, "GOROOT": {},
	"INFOPATH": {}, "MANPATH": {}, "PERL5LIB": {}, "PYTHONHOME": {}, "PYTHONPATH": {},
	"RUBYLIB": {}, "BUN_INSTALL": {}, "CURL_CA_BUNDLE": {}, "REQUESTS_CA_BUNDLE": {},
}

var excludedCallerEnvironmentPrefixes = []string{
	"ATUIN_", "CODELLDB_", "CODEX_", "COPILOT_", "GEMINI_CLI_IDE_", "GHOSTTY_",
	"HOMEBREW_", "MISE_", "NIX_", "NODE_REPL_TRUSTED_", "STARSHIP_", "SWIFTLY_",
	"TERM_", "VSCODE_", "VOLTA_", "XDG_", "XPC_",
}

var excludedCallerEnvironmentSuffixes = []string{
	"_PATH", "_PATHS", "_DIR", "_DIRS", "_HOME", "_ROOT", "_PREFIX", "_CELLAR",
	"_REPOSITORY", "_FILE", "_SOCK",
}

// EnvironmentOptions describes caller pass-through and explicit precedence.
type EnvironmentOptions struct {
	Caller        []string
	NoPassthrough bool
	Files         []map[string]string
	Explicit      []string
	Lookup        func(string) (string, bool)
}

// BuildProcessEnvironment applies rc's documented environment precedence.
func BuildProcessEnvironment(options EnvironmentOptions) (map[string]string, error) {
	caller := parseEnvironment(options.Caller)
	values := make(map[string]string)
	if !options.NoPassthrough {
		for name, value := range caller {
			if callerEnvironmentExcluded(name) {
				continue
			}
			values[name] = value
		}
	}
	for _, file := range options.Files {
		for name, value := range file {
			if !environmentNamePattern.MatchString(name) {
				return nil, fmt.Errorf("invalid environment variable name %q in env file", name)
			}
			values[name] = value
		}
	}
	lookup := options.Lookup
	if lookup == nil {
		lookup = func(name string) (string, bool) {
			value, ok := caller[name]
			return value, ok
		}
	}
	for _, entry := range options.Explicit {
		name, value, hasValue := strings.Cut(entry, "=")
		if !environmentNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if !hasValue {
			var ok bool
			value, ok = lookup(name)
			if !ok {
				return nil, fmt.Errorf("environment variable %s is not set", name)
			}
		}
		values[name] = value
	}

	return values, nil
}

func parseEnvironment(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok && environmentNamePattern.MatchString(name) {
			values[name] = value
		}
	}

	return values
}

func callerEnvironmentExcluded(name string) bool {
	if _, excluded := excludedCallerEnvironment[name]; excluded {
		return true
	}
	if strings.HasPrefix(name, "_") {
		return true
	}
	for _, prefix := range excludedCallerEnvironmentPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, suffix := range excludedCallerEnvironmentSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return strings.HasPrefix(name, "KUBERNETES_") || strings.HasPrefix(name, "RC_")
}

func ReadEnvironmentFiles(paths []string) ([]map[string]string, error) {
	files := make([]map[string]string, 0, len(paths))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open env file %s: %w", path, err)
		}
		values := make(map[string]string)
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			name, value, ok := strings.Cut(line, "=")
			if !ok || !environmentNamePattern.MatchString(name) {
				_ = file.Close()
				return nil, fmt.Errorf("invalid env file entry %q in %s", line, path)
			}
			values[name] = value
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("read env file %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close env file %s: %w", path, err)
		}
		files = append(files, values)
	}

	return files, nil
}
