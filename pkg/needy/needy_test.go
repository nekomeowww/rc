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

	"github.com/nekomeowww/rc/pkg/needy/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	decoded, err := profile.Decode([]byte(composed))
	require.NoError(t, err)
	require.NotNil(t, decoded.Extends)
	assert.Equal(t, "builtin:opencode", *decoded.Extends)
	assert.Equal(t, &profile.RunnerSelector{Name: "actor-1"}, decoded.Actor.Runner)
	require.NotNil(t, decoded.Actor.Workspace, "the profile's own settings are kept")
	native := profile.ExecutorDriverNative
	assert.Equal(t, &profile.RunnerSelector{Name: testWorkspaceRunner}, decoded.Executors["ws"].Runner)
	assert.Equal(t, &native, decoded.Executors["ws"].Driver)
	assert.Equal(t, &profile.RunnerSelector{Name: "ws-build"}, decoded.Executors["build"].Runner)
	assert.Equal(t, &native, decoded.Executors["build"].Driver, "a binding the profile does not name is native")
	assert.Len(t, decoded.Commands, 1)
}

func TestComposeProfileBuiltin(t *testing.T) {
	composed, err := ComposeProfile(BuiltinProfile("codex"), "actor-1", nil)
	require.NoError(t, err)

	decoded, err := profile.Decode([]byte(composed))
	require.NoError(t, err)
	assert.Equal(t, "builtin:codex", *decoded.Extends)
	assert.Equal(t, &profile.RunnerSelector{Name: "actor-1"}, decoded.Actor.Runner)
	assert.Empty(t, decoded.Executors)
}

func TestComposeProfileRefusesUnsafeProfiles(t *testing.T) {
	const parent = "extends: builtin:opencode\n"
	cases := map[string]struct {
		text     string
		bindings map[string]string
		message  string
	}{
		"actor runner": {
			text:    parent + "actor:\n  runner: somewhere\n",
			message: "actor.runner",
		},
		"actor runner by labels": {
			text:    parent + "actor:\n  runner: { labels: [kvm] }\n",
			message: "actor.runner",
		},
		"executor runner": {
			text:     parent + "executors:\n  ws:\n    runner: somewhere\n",
			bindings: map[string]string{"ws": testWorkspaceRunner},
			message:  "executor ws sets runner",
		},
		"unbound executor": {
			text:    parent + "executors:\n  ws:\n    driver: native\n",
			message: "executor ws is not bound",
		},
		"network services": {
			text:    parent + "actor:\n  network:\n    services:\n      api:\n        url: http://kubernetes.default.svc\n",
			message: "actor.network.services",
		},
		"unknown key": {
			text:    parent + "actor:\n  runnr: somewhere\n",
			message: "not valid",
		},
		"invalid YAML": {
			text:    "actor: {",
			message: "not valid",
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

func TestSecrets(t *testing.T) {
	secret, err := NewSecret()
	require.NoError(t, err)
	assert.Len(t, secret, 43, "32 bytes in unpadded base64url")
	assert.True(t, IsSecret(secret))
	other, err := NewSecret()
	require.NoError(t, err)
	assert.NotEqual(t, secret, other)

	assert.False(t, IsSecret(""))
	assert.False(t, IsSecret(secret[:40]), "fewer than 32 bytes")
	prefixed := "needy-runner.ws-dev." + strings.Repeat("a", 64)
	assert.False(t, IsSecret(prefixed), "a prefixed credential is not a plain secret")

	sum := sha256.Sum256([]byte(secret))
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), Hash(secret))
}

func TestMintedCredentials(t *testing.T) {
	minted := "needy-runner.actor-1." + strings.Repeat("a", 64)
	assert.True(t, IsMintedCredential(minted, "actor-1"))
	assert.False(t, IsMintedCredential(minted, "actor"))
}

func TestDaemonAndRunnerCommands(t *testing.T) {
	daemon := DaemonServeCommand(DaemonServeOptions{
		ControlListen:     "tcp://0.0.0.0:7400",
		GatewayListen:     "tcp://0.0.0.0:7401",
		RunnersFile:       "/files/runners.yaml",
		ClientsFile:       "/files/clients.yaml",
		GatewayBlockCIDRs: []string{"10.0.0.0/8"},
	})
	assert.Equal(t, "needy daemon serve --listen tcp://0.0.0.0:7400 --gateway-listen tcp://0.0.0.0:7401 "+
		"--runners-file /files/runners.yaml --clients-file /files/clients.yaml --disable-host-files "+
		"--gateway-block-cidr 10.0.0.0/8", strings.Join(daemon, " "))

	actor := ActorRunnerCommand(ActorRunnerOptions{
		Address:        "tcp://daemon:7400",
		Gateway:        "tcp://daemon:7401",
		CredentialFile: "/credential",
		WorkDirectory:  "/vm",
	})
	assert.Equal(t, "needy runner serve --address tcp://daemon:7400 --credential-file /credential "+
		"--roles actor --ephemeral --gateway tcp://daemon:7401 --work-dir /vm", strings.Join(actor, " "))

	executor := ExecutorRunnerCommand(ExecutorRunnerOptions{
		Executable:         "/bin/needy",
		Name:               "ws-dev",
		Address:            "tcp://daemon:7400",
		CredentialVariable: "CREDENTIAL",
		ActivityHook:       "touch",
	})
	assert.Equal(t, "/bin/needy runner serve --address tcp://daemon:7400 --name ws-dev --credential-env CREDENTIAL "+
		"--roles executor --activity-hook touch", strings.Join(executor, " "))
}

func TestStaticClients(t *testing.T) {
	token, err := NewSecret()
	require.NoError(t, err)

	data, err := ClientsFile([]StaticClient{
		{
			Name:      "rc-controller",
			Access:    AccessAdmin,
			TokenHash: Hash(token),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "clients:\n- access: admin\n  name: rc-controller\n  token_hash: "+Hash(token)+"\n", string(data))
}

func TestRunnersFileIsSorted(t *testing.T) {
	data, err := RunnersFile([]StaticRunner{
		{
			Name:           "ws-b",
			CredentialHash: "sha256:b",
			Roles:          []Role{RoleExecutor},
		},
		{
			Name:           "ws-a",
			CredentialHash: "sha256:a",
			Roles:          []Role{RoleExecutor},
			Labels:         map[string]string{"example.com/workspace": "a"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, `runners:
- credential_hash: sha256:a
  labels:
    example.com/workspace: a
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
