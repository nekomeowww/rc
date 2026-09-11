package workspaces

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/nekomeowww/rc/internal/lifecycle"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

const (
	testLinuxImage        = "example/linux"
	testWindowsExecutable = "rc-kube.exe"
	testWindowsEndpoint   = `\\.\pipe\rc-kube`
)

func TestWindowsWorkspaceRuntime(t *testing.T) {
	t.Parallel()
	assertions, requirements := assert.New(t), require.New(t)
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "electron", Namespace: "runtime-test"},
		Spec:       workspacesv1alpha1.WorkspaceSpec{OS: corev1.Windows, NodeSelector: map[string]string{"pool": "desktop"}, Tolerations: []corev1.Toleration{{Key: "os", Value: "windows", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule}}},
	}
	action := lifecycle.Action{Script: `New-Item -ItemType Directory C:\workspace\app`}
	resolved := &resolvedWorkspace{image: "example/runner:windows", initializers: []workspaceInitializer{{name: "checkout", image: "example/runner:windows", action: action}}, beforeStop: []lifecycle.Action{{Command: []string{"node.exe", "cleanup.cjs"}}}}
	pod, err := workspaceRuntimePod(workspace, resolved)
	requirements.NoError(err)
	assertions.Equal(corev1.Windows, pod.Spec.OS.Name)
	assertions.Equal("windows", pod.Spec.NodeSelector[corev1.LabelOSStable])
	assertions.Equal("desktop", pod.Spec.NodeSelector["pool"])
	assertions.Equal(workspace.Spec.Tolerations, pod.Spec.Tolerations)
	assertions.Nil(pod.Spec.SecurityContext.RunAsUser)
	assertions.Nil(pod.Spec.SecurityContext.FSGroup)
	assertions.Nil(pod.Spec.SecurityContext.SeccompProfile)
	assertions.Empty(pod.Spec.InitContainers, "local checkout must be prepared in the same writable layer as the runtime")
	requirements.Len(pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assertions.Empty(container.Command, "retain image entrypoint session initialization")
	assertions.Equal([]string{testWindowsExecutable, "serve", "--socket", testWindowsEndpoint, "--state-dir", `C:\home\agent\.rc\processes`, "--initialize-actions"}, container.Args[:7])
	actions, err := lifecycle.Decode(container.Args[7])
	requirements.NoError(err)
	assertions.Equal([]lifecycle.Action{action}, actions)
	assertions.Nil(container.SecurityContext)
	assertions.Equal([]string{testWindowsExecutable, "health", "--socket", testWindowsEndpoint}, container.ReadinessProbe.Exec.Command)
	assertions.Equal(testWindowsExecutable, container.Lifecycle.PreStop.Exec.Command[0])
	assertions.Equal(`C:\home\agent`, container.VolumeMounts[0].MountPath)
	for _, mount := range container.VolumeMounts {
		assertions.NotEqual(`C:\workspace`, mount.MountPath, "preserve a local filesystem for build caches")
	}
}

func TestRuntimeOSValidationAndDefaultImage(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme))
	reconciler := &WorkspaceReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), RunnerImage: testLinuxImage, WindowsRunnerImage: "example/windows"}
	workspace := &workspacesv1alpha1.Workspace{Spec: workspacesv1alpha1.WorkspaceSpec{OS: corev1.Windows, Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")}}}
	resolved, reason, _, err := reconciler.resolveWorkspaceBase(context.Background(), workspace)
	requirements.NoError(err)
	requirements.Empty(reason)
	assert.Equal(t, "example/windows", resolved.image)
	workspace.Spec.NodeSelector = map[string]string{corev1.LabelOSStable: "linux"}
	_, reason, _, err = reconciler.resolveWorkspaceBase(context.Background(), workspace)
	requirements.NoError(err)
	assert.Equal(t, "OSPlacementConflict", reason)
	workspace.Spec.NodeSelector = nil
	workspace.Spec.Mounts = []workspacesv1alpha1.WorkspaceMount{{Name: "source", Path: "source"}}
	_, reason, _, err = reconciler.resolveWorkspaceBase(context.Background(), workspace)
	requirements.NoError(err)
	assert.Equal(t, "UnsupportedWindowsMount", reason, "fail explicitly before provisioning an incompatible Linux Git mount")
}

func TestDarwinRuntimeUsesConfiguredHostStorage(t *testing.T) {
	t.Parallel()
	reconciler := &WorkspaceReconciler{
		Client: fake.NewClientBuilder().Build(), RunnerImage: testLinuxImage,
		DarwinRunnerImage: "example/macos:26.3", DarwinWorkspaceRoot: "/Users/runner/.local/share/rc",
	}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "xcode", Namespace: "development"},
		Spec:       workspacesv1alpha1.WorkspaceSpec{OS: rcplatform.Darwin},
	}
	resolved, reason, _, err := reconciler.resolveWorkspaceBase(context.Background(), workspace)
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Equal(t, "example/macos:26.3", resolved.image)
	assert.Equal(t, "/Users/runner/.local/share/rc/development/xcode", resolved.homeHostPath)
	pod, err := workspaceRuntimePod(workspace, resolved)
	require.NoError(t, err)
	assert.Nil(t, pod.Spec.OS)
	assert.Equal(t, "darwin", pod.Spec.NodeSelector[corev1.LabelOSStable])
	assert.Equal(t, resolved.homeHostPath, pod.Spec.Volumes[0].HostPath.Path)
}

func TestWindowsEnvironmentEditor(t *testing.T) {
	t.Parallel()
	environment := &workspacesv1alpha1.WorkspaceEnvironment{Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{OS: corev1.Windows, Image: "example/windows"}}
	pod, err := environmentEditorPod(environment, "draft")
	require.NoError(t, err)
	assert.Equal(t, corev1.Windows, pod.Spec.OS.Name)
	assert.Empty(t, pod.Spec.InitContainers, "Windows has no sudoers initialization")
	assert.Nil(t, pod.Spec.SecurityContext.FSGroup)
	assert.Equal(t, `C:\home\agent`, pod.Spec.Containers[0].VolumeMounts[0].MountPath)
	assert.Equal(t, "draft", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
}

func TestWindowsProcessRequestIsLogical(t *testing.T) {
	t.Parallel()
	process := &workspacesv1alpha1.AgentProcess{ObjectMeta: metav1.ObjectMeta{Name: "electron"}, Spec: workspacesv1alpha1.AgentProcessSpec{Command: []string{"electron.exe", "main.cjs"}}}
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: corev1.Windows})
	require.NoError(t, err)
	target := &resolvedProcessTarget{platform: platform, runtime: processruntime.Target{Executable: testWindowsExecutable, Endpoint: testWindowsEndpoint}, defaultDirectory: rcplatform.WorkspaceDirectory}
	request, err := (&AgentProcessReconciler{}).processStartRequest(context.Background(), process, target)
	require.NoError(t, err)
	assert.Empty(t, request.WorkingDirectory)
	assert.Equal(t, processruntime.DefaultDirectoryWorkspace, request.DefaultDirectory)
	assert.Empty(t, request.RuntimeDirectory)
	assert.Empty(t, request.TranscriptPath)
}
