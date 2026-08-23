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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProcessEnvironmentExcludesRuntimeAndLocalPathValues(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)

	values, err := BuildProcessEnvironment(EnvironmentOptions{
		Caller: []string{
			"EDITOR=vim", "LANG=en_US.UTF-8", "PATH=/local/bin", "KUBECONFIG=/tmp/kubeconfig",
			"KUBERNETES_SERVICE_HOST=127.0.0.1", "CODEX_HOME=/local/codex", "RC_INTERNAL=value",
		},
		Files:    []map[string]string{{"EDITOR": "nano", "FROM_FILE": "yes"}},
		Explicit: []string{"PATH=/workspace/bin", "TOKEN"},
		Lookup: func(name string) (string, bool) {
			if name == "TOKEN" {
				return "selected", true
			}
			return "", false
		},
	})
	requirements.NoError(err, "build process environment")
	assertions.Equal("en_US.UTF-8", values["LANG"], "pass through ordinary caller values")
	assertions.Equal("nano", values["EDITOR"], "env file overrides caller")
	assertions.Equal("yes", values["FROM_FILE"], "include env file values")
	assertions.Equal("/workspace/bin", values["PATH"], "explicit values may restore excluded names")
	assertions.Equal("selected", values["TOKEN"], "explicit name reads caller value")
	assertions.NotContains(values, "KUBECONFIG", "exclude caller Kubernetes client configuration")
	assertions.NotContains(values, "KUBERNETES_SERVICE_HOST", "exclude Kubernetes wildcard")
	assertions.NotContains(values, "CODEX_HOME", "exclude caller agent home")
	assertions.NotContains(values, "RC_INTERNAL", "exclude rc runtime values")
}

func TestBuildProcessEnvironmentExcludesHostRuntimeAndFilesystemValues(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)

	values, err := BuildProcessEnvironment(EnvironmentOptions{
		Caller: []string{
			"TMPDIR=/var/folders/host/T", "TMP=/private/tmp", "TEMP=/private/tmp",
			"HOMEBREW_PREFIX=/opt/homebrew", "PNPM_HOME=/Users/person/Library/pnpm",
			"CUSTOM_CONFIG_FILE=/Users/person/.config/tool", "XDG_DATA_DIRS=/opt/homebrew/share",
			"GIT_ASKPASS=/Applications/Editor.app/askpass", "PYTHONSTARTUP=/Users/person/.pythonrc",
			"NODE_OPTIONS=--require /Users/person/runtime-hook.js",
			"VSCODE_GIT_IPC_HANDLE=/var/folders/host/vscode.sock", "GEMINI_CLI_IDE_AUTH_TOKEN=local-token",
			"CODEX_THREAD_ID=local-thread", "ATUIN_SESSION=local-session", "__MISE_DIFF=local-state",
			"AWS_PROFILE=development", "HTTP_PROXY=http://proxy.example:8080",
		},
	})
	requirements.NoError(err, "build process environment")

	for _, name := range []string{
		"TMPDIR", "TMP", "TEMP", "HOMEBREW_PREFIX", "PNPM_HOME", "CUSTOM_CONFIG_FILE",
		"XDG_DATA_DIRS", "GIT_ASKPASS", "PYTHONSTARTUP", "NODE_OPTIONS", "VSCODE_GIT_IPC_HANDLE",
		"GEMINI_CLI_IDE_AUTH_TOKEN", "CODEX_THREAD_ID", "ATUIN_SESSION", "__MISE_DIFF",
	} {
		assertions.NotContains(values, name, "exclude host-only environment variable %s", name)
	}
	assertions.Equal("development", values["AWS_PROFILE"], "preserve an ordinary caller setting")
	assertions.Equal("http://proxy.example:8080", values["HTTP_PROXY"], "preserve an ordinary network setting")
}

func TestBuildProcessEnvironmentExplicitlyRestoresExcludedHostVariable(t *testing.T) {
	t.Parallel()

	values, err := BuildProcessEnvironment(EnvironmentOptions{
		Caller:   []string{"TMPDIR=/var/folders/host/T"},
		Explicit: []string{"TMPDIR=/tmp"},
	})
	require.NoError(t, err, "build process environment")
	assert.Equal(t, "/tmp", values["TMPDIR"], "allow an explicit container-local temporary directory")
}

func TestBuildProcessEnvironmentCanDisablePassthrough(t *testing.T) {
	t.Parallel()
	values, err := BuildProcessEnvironment(EnvironmentOptions{
		Caller:        []string{"LANG=en_US.UTF-8"},
		NoPassthrough: true,
		Explicit:      []string{"LANG=zh_CN.UTF-8"},
	})
	require.NoError(t, err, "build explicit-only environment")
	assert.Equal(t, map[string]string{"LANG": "zh_CN.UTF-8"}, values)
}
