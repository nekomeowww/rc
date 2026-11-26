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

package needy

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const testWorkspaceRunner = "ws-dev"

func TestComposeProfilePlacesEveryPart(t *testing.T) {
	text := `extends: builtin:opencode
actor:
  workspace:
    from: "executor:ws:~/project"
executors:
  ws:
    driver: native
commands:
  - match: "**"
    to: ws
`
	composed, err := ComposeProfile(text, "actor-1", map[string]string{
		"ws":    testWorkspaceRunner,
		"build": "ws-build",
	})
	require.NoError(t, err)

	var profile map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(composed), &profile))
	assert.Equal(t, "builtin:opencode", profile["extends"])
	actor := profile["actor"].(map[string]any)
	assert.Equal(t, "actor-1", actor["runner"])
	assert.Equal(t, "executor:ws:~/project", actor["workspace"].(map[string]any)["from"])
	executors := profile["executors"].(map[string]any)
	assert.Equal(t, map[string]any{profileDriver: profileNativeDriver, profileRunner: testWorkspaceRunner}, executors["ws"])
	assert.Equal(t, map[string]any{profileDriver: profileNativeDriver, profileRunner: "ws-build"}, executors["build"])
	assert.Len(t, profile["commands"], 1)
}

func TestComposeProfileBuiltin(t *testing.T) {
	composed, err := ComposeProfile(BuiltinProfile("codex"), "actor-1", nil)
	require.NoError(t, err)

	var profile map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(composed), &profile))
	assert.Equal(t, "builtin:codex", profile["extends"])
	assert.Equal(t, map[string]any{profileRunner: "actor-1"}, profile["actor"])
	assert.NotContains(t, profile, "executors")
}

func TestComposeProfileRefusesUnsafeProfiles(t *testing.T) {
	cases := map[string]struct {
		text     string
		bindings map[string]string
		message  string
	}{
		"actor runner": {
			text:    "actor:\n  runner: somewhere\n",
			message: "actor.runner",
		},
		"executor runner": {
			text:     "executors:\n  ws:\n    runner: somewhere\n",
			bindings: map[string]string{"ws": testWorkspaceRunner},
			message:  "executor ws sets runner",
		},
		"unbound executor": {
			text:    "executors:\n  ws:\n    driver: native\n",
			message: "executor ws is not bound",
		},
		"network services": {
			text:    "actor:\n  network:\n    services:\n      api:\n        url: http://kubernetes.default.svc\n",
			message: "actor.network.services",
		},
		"not a mapping": {
			text:    "actor: [1]\n",
			message: "actor must be a mapping",
		},
		"invalid YAML": {
			text:    "actor: {",
			message: "not valid YAML",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ComposeProfile(test.text, "actor-1", test.bindings)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.message)
		})
	}
}

func TestRunnerCredentials(t *testing.T) {
	credential, err := NewRunnerCredential(testWorkspaceRunner)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(credential, "needy-runner.ws-dev."))
	assert.Len(t, strings.TrimPrefix(credential, "needy-runner.ws-dev."), 64)
	assert.True(t, CredentialMatchesRunner(credential, testWorkspaceRunner))
	assert.False(t, CredentialMatchesRunner(credential, "ws-de"))

	sum := sha256.Sum256([]byte(credential))
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), CredentialHash(credential))
}

func TestStaticClients(t *testing.T) {
	token, err := NewClientToken(ControllerClientName)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, "needy-client.static-rc-controller."))
	assert.Len(t, strings.TrimPrefix(token, "needy-client.static-rc-controller."), 64)
	assert.True(t, TokenMatchesClient(token, ControllerClientName))
	assert.False(t, TokenMatchesClient(token, "rc"))

	data, err := ClientsFile([]StaticClient{
		{
			Name:      ControllerClientName,
			Access:    "admin",
			TokenHash: CredentialHash(token),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "clients:\n- access: admin\n  name: rc-controller\n  token_hash: "+CredentialHash(token)+"\n", string(data))
}

func TestRunnerNamesAreDNSLabels(t *testing.T) {
	assert.Equal(t, testWorkspaceRunner, WorkspaceRunnerName("dev"))
	long := WorkspaceRunnerName(strings.Repeat("a", 80))
	assert.Len(t, long, 63)
	assert.NotEqual(t, long, WorkspaceRunnerName(strings.Repeat("a", 79)))
	assert.Equal(t, "actor-0123456789ab", ActorRunnerName("01234567-89ab-cdef-0123-456789abcdef"))
}

func TestRunnersFileIsSorted(t *testing.T) {
	data, err := RunnersFile([]StaticRunner{
		{
			Name:           "ws-b",
			CredentialHash: "sha256:b",
			Roles:          []string{"executor"},
		},
		{
			Name:           "ws-a",
			CredentialHash: "sha256:a",
			Roles:          []string{"executor"},
			Labels:         map[string]string{WorkspaceLabel: "a"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, `runners:
- credential_hash: sha256:a
  labels:
    rc.ayaka.io/workspace: a
  name: ws-a
  roles:
  - executor
- credential_hash: sha256:b
  name: ws-b
  roles:
  - executor
`, string(data))

	empty, err := RunnersFile(nil)
	require.NoError(t, err)
	assert.Equal(t, "runners: []\n", string(empty))
}

func TestParseKVMPlacement(t *testing.T) {
	selector, err := ParseSelector("example.com/kvm=true, zone=a")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"example.com/kvm": "true",
		"zone":            "a",
	}, selector)
	_, err = ParseSelector("missing-value")
	require.Error(t, err)

	tolerations, err := ParseTolerations("example.com/kvm=true:NoSchedule,dedicated:NoExecute")
	require.NoError(t, err)
	assert.Equal(t, []corev1.Toleration{
		{
			Key:      "example.com/kvm",
			Operator: corev1.TolerationOpEqual,
			Value:    "true",
			Effect:   corev1.TaintEffectNoSchedule,
		},
		{
			Key:      "dedicated",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoExecute,
		},
	}, tolerations)
	_, err = ParseTolerations("no-effect")
	require.Error(t, err)
}
