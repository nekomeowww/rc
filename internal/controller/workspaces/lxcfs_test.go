package workspaces

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	testLXCFSDir          = "/var/lib/lxcfs"
	testLXCFSWindowsImage = "example/lxcfs-windows"
)

func lxcfsVolumeNames(pod *corev1.Pod) []string {
	names := make([]string, 0)
	for _, volume := range pod.Spec.Volumes {
		if strings.HasPrefix(volume.Name, "lxcfs-") {
			names = append(names, volume.Name)
		}
	}
	return names
}

func TestLXCFSDirAppliesOnlyToLinuxWorkspaces(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		os      corev1.OSName
		volumes int
	}{
		{
			name:    "linux",
			os:      corev1.Linux,
			volumes: 8,
		},
		{
			name:    "windows",
			os:      corev1.Windows,
			volumes: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requirements := require.New(t)
			scheme := runtime.NewScheme()
			requirements.NoError(workspacesv1alpha1.AddToScheme(scheme))
			kube := fake.NewClientBuilder().WithScheme(scheme).Build()
			reconciler := &WorkspaceReconciler{
				Client:             kube,
				APIReader:          kube,
				RunnerImage:        testLinuxImage,
				WindowsRunnerImage: testLXCFSWindowsImage,
				LXCFSDir:           testLXCFSDir,
			}
			workspace := &workspacesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "lxcfs", Namespace: "runtime-test", UID: "lxcfs-uid"},
				Spec: workspacesv1alpha1.WorkspaceSpec{
					OS:      test.os,
					Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")},
				},
			}
			resolved, reason, _, err := reconciler.resolveWorkspaceBase(context.Background(), workspace)
			requirements.NoError(err)
			requirements.Empty(reason)

			pod, err := workspaceRuntimePod(workspace, resolved)
			requirements.NoError(err)
			assert.Len(t, lxcfsVolumeNames(pod), test.volumes)
		})
	}
}

func TestLXCFSDirTopologyHash(t *testing.T) {
	t.Parallel()
	workspace := &workspacesv1alpha1.Workspace{}
	resolved := &resolvedWorkspace{runtime: testRCPlatform(t), image: testRunnerImage}

	baseHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	// The same hash as before LXCFS support existed. An empty directory must
	// not replace existing runtime Pods.
	assert.Equal(t, "965105d34b7fc53be3e718b27fe1284cdbc1097ae9a87b9d8edca936d5241004", baseHash)

	resolved.lxcfsDir = testLXCFSDir
	lxcfsHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, lxcfsHash, "enabling LXCFS changes the Pod volumes and the topology")
}

func TestEnablingLXCFSRollsIdleRuntime(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	fixture := newPodMetadataFixture(t, workspacesv1alpha1.WorkspaceSpec{})

	fixture.reconcile(t)
	pod := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, pod), "create the runtime Pod")
	assertions.Empty(lxcfsVolumeNames(pod))
	fixture.reconcile(t)
	assertions.Zero(fixture.podDeletes.Load(), "keep the runtime Pod while LXCFS is disabled")

	fixture.reconciler.LXCFSDir = testLXCFSDir
	fixture.reconcile(t)
	assertions.True(apierrors.IsNotFound(fixture.client.Get(ctx, fixture.key, new(corev1.Pod))), "replace the idle runtime Pod")
	assertions.Equal("Replacing", fixture.readyReason(t))

	fixture.reconcile(t)
	replacement := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, replacement), "create the replacement runtime Pod")
	assertions.Len(lxcfsVolumeNames(replacement), 8, "the replacement mounts the LXCFS files")
}

func TestLXCFSReservesMountNamePrefix(t *testing.T) {
	t.Parallel()
	workspace := &workspacesv1alpha1.Workspace{
		Spec: workspacesv1alpha1.WorkspaceSpec{
			Mounts: []workspacesv1alpha1.WorkspaceMount{
				{
					Name: "lxcfs-proc-stat",
					Path: "src",
				},
			},
		},
	}
	reconciler := &WorkspaceReconciler{}
	resolved := &resolvedWorkspace{runtime: testRCPlatform(t)}

	_, reason, _, err := reconciler.resolveWorkspaceDependencies(context.Background(), workspace, resolved)
	require.NoError(t, err)
	assert.NotEqual(t, "ReservedMountName", reason, "allow the name while LXCFS is disabled")

	resolved.lxcfsDir = testLXCFSDir
	_, reason, _, err = reconciler.resolveWorkspaceDependencies(context.Background(), workspace, resolved)
	require.NoError(t, err)
	assert.Equal(t, "ReservedMountName", reason, "reject a mount name that collides with an LXCFS volume")
}
