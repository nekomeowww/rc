package workspaces

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/rcplatform"
	"github.com/nekomeowww/rc/internal/version"
)

const (
	// testLinuxTopologyHash is the topology hash of a minimal Linux runtime
	// that runs the injected rc-kube.
	testLinuxTopologyHash = "f816a5923eb4dc023db2c9887a5e065fb11cc57ecb2eb29a59a44540c5297008"
	// testImageRCKubeTopologyHash is the hash of the same runtime when it ran
	// the rc-kube of its image.
	testImageRCKubeTopologyHash = "965105d34b7fc53be3e718b27fe1284cdbc1097ae9a87b9d8edca936d5241004"
)

func TestRCKubeInjectionTopologyHash(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		os   corev1.OSName
		hash string
	}{
		{
			name: "LinuxMovesToTheInjectedRCKubeOnce",
			os:   corev1.Linux,
			hash: testLinuxTopologyHash,
		},
		{
			name: "WindowsIsUnchanged",
			os:   corev1.Windows,
			hash: "5a31319d94b56870d6257c8024469736996bc43f9e696acca959a46a7abfa61b",
		},
		{
			name: "DarwinIsUnchanged",
			os:   rcplatform.Darwin,
			hash: "e94864668b1789240ba822b3113994947a909c42ba790c7d830ff9beaca7f845",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			platform, err := rcplatform.Resolve(rcplatform.Target{OS: test.os})
			require.NoError(t, err)
			workspace := &workspacesv1alpha1.Workspace{}
			resolved := &resolvedWorkspace{runtime: platform, image: testRunnerImage}

			hash, err := workspaceTopologyHash(workspace, resolved)
			require.NoError(t, err)
			assert.Equal(t, test.hash, hash)
			if test.os == corev1.Linux {
				assert.NotEqual(t, testImageRCKubeTopologyHash, hash, "replace a runtime that runs the rc-kube of its image")
			}

			resolved.rcKubeImage = "registry.example.com/rc/rc-kube:9.9.9"
			changed, err := workspaceTopologyHash(workspace, resolved)
			require.NoError(t, err)
			assert.Equal(t, hash, changed, "a new controller version does not replace running Pods")
		})
	}
}

func installImage(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	require.NotEmpty(t, pod.Spec.InitContainers)
	require.Equal(t, rcplatform.RCKubeInstallContainerName, pod.Spec.InitContainers[0].Name)

	return pod.Spec.InitContainers[0].Image
}

func TestWorkspaceRuntimeInstallsTheRCKubeOfTheController(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newPodMetadataFixture(t, workspacesv1alpha1.WorkspaceSpec{})

	fixture.reconcile(t)
	pod := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(ctx, fixture.key, pod))
	assert.Equal(t, version.DefaultRCKubeImage(), installImage(t, pod), "without a configured image, use the image of the controller version")
	assert.Equal(t, testRunnerImage, pod.Spec.Containers[0].Image, "the Workspace image still provides the userland")
	assert.Equal(t, rcplatform.RCKubeExecutable, pod.Spec.Containers[0].Command[0])
}

func TestRCKubeImageChangeKeepsRunningRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newPodMetadataFixture(t, workspacesv1alpha1.WorkspaceSpec{})
	fixture.reconciler.RCKubeImage = testRCKubeImage

	fixture.reconcile(t)
	pod := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(ctx, fixture.key, pod))
	assert.Equal(t, testRCKubeImage, installImage(t, pod), "use the configured image")

	fixture.reconciler.RCKubeImage = "registry.example.com/rc/rc-kube:9.9.9"
	fixture.reconcile(t)
	assert.Zero(t, fixture.podDeletes.Load(), "a controller upgrade does not replace the running runtime")
	require.NoError(t, fixture.client.Get(ctx, fixture.key, pod))
	assert.Equal(t, testRCKubeImage, installImage(t, pod), "the running runtime keeps the rc-kube it started with")
}
