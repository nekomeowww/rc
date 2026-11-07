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

package rckube

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testKubernetesHost     = "KUBERNETES_SERVICE_HOST"
	testKubernetesPort     = "KUBERNETES_PORT_443_TCP"
	testKubernetesExplicit = "KUBERNETES_EXPLICIT"
	testOrdinaryVariable   = "RC_TEST_ORDINARY"
)

// setKubeletEnvironment sets values like the ones the kubelet adds to every
// container. t.Setenv restores them when the test ends.
func setKubeletEnvironment(t *testing.T) {
	t.Setenv(testKubernetesHost, "10.0.0.1")
	t.Setenv(testKubernetesPort, "tcp://10.0.0.1:443")
	t.Setenv(testKubernetesExplicit, "from-kubelet")
	t.Setenv(testOrdinaryVariable, "kept")
}

func environmentMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}

	return values
}

func TestHideKubernetesEnvironmentRemovesInheritedVariables(t *testing.T) {
	assertions := assert.New(t)
	setKubeletEnvironment(t)

	require.NoError(t, HideKubernetesEnvironment())
	// The explicit value stands for a spec.env variable, which reaches a
	// process in its start request and not in the inherited environment.
	environment, err := mergedEnvironment(map[string]string{testKubernetesExplicit: "from-spec"})
	require.NoError(t, err)

	values := environmentMap(environment)
	for name := range values {
		if name != testKubernetesExplicit {
			assertions.NotContains(name, "KUBERNETES_", "remove every inherited Kubernetes variable")
		}
	}
	assertions.Equal("from-spec", values[testKubernetesExplicit], "keep a Kubernetes variable the Workspace sets")
	assertions.Equal("kept", values[testOrdinaryVariable], "keep other inherited variables")
}

func TestKubernetesEnvironmentStaysWithoutHide(t *testing.T) {
	assertions := assert.New(t)
	setKubeletEnvironment(t)

	environment, err := mergedEnvironment(nil)
	require.NoError(t, err)

	values := environmentMap(environment)
	assertions.Equal("10.0.0.1", values[testKubernetesHost], "keep the API server host for in-cluster clients")
	assertions.Equal("tcp://10.0.0.1:443", values[testKubernetesPort], "keep the API server port for in-cluster clients")
	assertions.Equal("from-kubelet", values[testKubernetesExplicit])
}
