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

package rcplatform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWorkspacePodDataPlaneOptIn(t *testing.T) {
	rt, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)

	baseIntent := WorkspacePodIntent{
		Metadata:    metav1.ObjectMeta{Name: "demo", Namespace: "team-a"},
		Image:       "example.com/runner:latest",
		HomeClaim:   "demo-home",
		RCKubeImage: "example.com/rc-kube:latest",
	}

	withoutDP, err := rt.WorkspacePod(baseIntent)
	require.NoError(t, err)
	assert.Empty(t, withoutDP.Spec.Containers[0].Ports)
	assert.NotContains(t, withoutDP.Spec.Containers[0].Args, "--data-plane-listen")

	withDPIntent := baseIntent
	withDPIntent.DataPlane = &WorkspaceDataPlanePodConfig{
		CertSecretName: "demo-data-plane-tls",
	}
	withDP, err := rt.WorkspacePod(withDPIntent)
	require.NoError(t, err)

	require.Len(t, withDP.Spec.Containers[0].Ports, 2)
	assert.Equal(t, int32(44772), withDP.Spec.Containers[0].Ports[0].ContainerPort)
	assert.Equal(t, int32(49983), withDP.Spec.Containers[0].Ports[1].ContainerPort)
	assert.Contains(t, withDP.Spec.Containers[0].Args, "--execd-listen")
	assert.Contains(t, withDP.Spec.Containers[0].Args, ":44772")
	assert.Contains(t, withDP.Spec.Containers[0].Args, "--data-plane-listen")
	assert.Contains(t, withDP.Spec.Containers[0].Args, ":49983")
	assert.Contains(t, withDP.Spec.Containers[0].Args, "--data-plane-ca-file")
	assert.Contains(t, withDP.Spec.Containers[0].Args, "--data-plane-cert-file")
	assert.Contains(t, withDP.Spec.Containers[0].Args, "--data-plane-key-file")

	foundVol := false
	for _, vol := range withDP.Spec.Volumes {
		if vol.Name == dataPlaneVolumeName && vol.Secret != nil && vol.Secret.SecretName == "demo-data-plane-tls" {
			foundVol = true
		}
	}
	assert.True(t, foundVol, "expected data plane TLS secret volume")
}
