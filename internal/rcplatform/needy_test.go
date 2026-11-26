package rcplatform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

const testNeedyImage = "example.com/needy:1"

func TestLinuxWorkspacePodInstallsNeedy(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)
	intent := testRCKubeIntent()
	intent.NeedyImage = testNeedyImage

	pod, err := runtime.WorkspacePod(intent)
	require.NoError(t, err)
	assertInstallsRCKube(t, pod)
	require.GreaterOrEqual(t, len(pod.Spec.InitContainers), 2)
	install := pod.Spec.InitContainers[1]
	assert.Equal(t, NeedyInstallContainerName, install.Name)
	assert.Equal(t, testNeedyImage, install.Image)
	assert.Equal(t, []string{"cp", "/usr/local/bin/needy", "/opt/needy/bin/needy"}, install.Command)
	volume := findVolume(pod, needyVolumeName)
	require.NotNil(t, volume)
	assert.NotNil(t, volume.EmptyDir)
	var mounted bool
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == needyVolumeName {
			mounted = true
			assert.Equal(t, "/opt/needy/bin", mount.MountPath)
			assert.True(t, mount.ReadOnly)
		}
	}
	assert.True(t, mounted, "the runtime container runs the installed needy")
}

func TestWorkspacePodWithoutNeedyInstallsNoNeedy(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)

	pod, err := runtime.WorkspacePod(testRCKubeIntent())
	require.NoError(t, err)
	assert.Nil(t, findVolume(pod, needyVolumeName))
	for _, container := range pod.Spec.InitContainers {
		assert.NotEqual(t, NeedyInstallContainerName, container.Name)
	}
}
