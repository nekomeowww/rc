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
