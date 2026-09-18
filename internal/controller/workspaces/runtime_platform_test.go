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

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/lifecycle"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

const (
	testLinuxImage        = "example/linux"
	testWindowsExecutable = "rc-kube.exe"
	testWindowsEndpoint   = `\\.\pipe\rc-kube`
	testWindowsMount      = "code"
)

func TestWindowsWorkspaceRuntime(t *testing.T) {
	t.Parallel()
	assertions, requirements := assert.New(t), require.New(t)
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "electron", Namespace: "runtime-test"},
		Spec:       workspacesv1alpha1.WorkspaceSpec{OS: corev1.Windows, NodeSelector: map[string]string{"pool": "desktop"}, Tolerations: []corev1.Toleration{{Key: "os", Value: string(corev1.Windows), Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule}}},
	}
	action := lifecycle.Action{Script: `New-Item -ItemType Directory C:\workspace\app`}
	resolved := &resolvedWorkspace{image: "example/runner:windows", initializers: []workspaceInitializer{{name: "checkout", image: "example/runner:windows", action: action}}, beforeStop: []lifecycle.Action{{Command: []string{"node.exe", "cleanup.cjs"}}}}
	pod, err := workspaceRuntimePod(workspace, resolved)
	requirements.NoError(err)
	assertions.Equal(corev1.Windows, pod.Spec.OS.Name)
	assertions.Equal(string(corev1.Windows), pod.Spec.NodeSelector[corev1.LabelOSStable])
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
	assert.Empty(t, reason, "Windows mounts are resolved against their source PVCs")
}

func TestWindowsLinkedWorktreeMountPaths(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "feature", Namespace: testNamespace},
		Status:     repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "child", WorktreePath: "/repository/worktree/feature", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue}}},
	}
	reconciler := &WorkspaceReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()}
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: corev1.Windows})
	require.NoError(t, err)
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace}, Spec: workspacesv1alpha1.WorkspaceSpec{OS: corev1.Windows, Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: testWindowsMount, Path: testWindowsMount, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "feature"}}}}}
	resolved, reason, _, err := reconciler.resolveWorkspaceDependencies(context.Background(), workspace, &resolvedWorkspace{runtime: platform})
	require.NoError(t, err)
	require.Empty(t, reason)
	// Git for Windows resolves the existing /mnt/... gitdir on drive C.
	// Preserve that hidden volume root instead of rewriting shared Git metadata.
	require.Len(t, resolved.volumeMounts, 2)
	assert.Equal(t, `C:\workspace\code`, resolved.volumeMounts[0].MountPath)
	assert.Equal(t, "worktree/feature", resolved.volumeMounts[0].SubPath)
	assert.Equal(t, `C:\mnt\rc\worktrees\feature`, resolved.volumeMounts[1].MountPath)
	require.Len(t, resolved.writeClaims, 1, "Windows retains the same exclusive Worktree lease")
	workspace.Spec.Mounts[0].ReadOnly = true
	resolved, reason, _, err = reconciler.resolveWorkspaceDependencies(context.Background(), workspace, &resolvedWorkspace{runtime: platform})
	require.NoError(t, err)
	require.Empty(t, reason)
	assert.Empty(t, resolved.writeClaims)
	for _, mount := range resolved.volumeMounts {
		assert.True(t, mount.ReadOnly, "the hidden metadata mount must also remain read-only")
	}
	for _, invalidPath := range []string{`..\escape`, `C:\escape`, "../escape"} {
		workspace.Spec.Mounts[0].Path = invalidPath
		_, reason, _, err = reconciler.resolveWorkspaceDependencies(context.Background(), workspace, &resolvedWorkspace{runtime: platform})
		require.NoError(t, err)
		assert.Equal(t, "InvalidMountPath", reason)
	}
}

func TestWindowsDeferredMountUsesMainRuntimeImage(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: "generated", Namespace: testNamespace, Labels: map[string]string{"workspaces.rc.ayaka.io/generated-for": string(corev1.Windows)}},
		Spec:       repositoriesv1alpha1.WorktreeSpec{Branch: "rc/generated"},
		Status:     repositoriesv1alpha1.WorktreeStatus{VolumeClaimName: "generated", WorktreePath: "/repository", Conditions: []metav1.Condition{{Type: repositoriesv1alpha1.WorktreeConditionVolumeReady, Status: metav1.ConditionTrue}}},
	}
	reconciler := &WorkspaceReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build(), RunnerImage: testLinuxImage}
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: corev1.Windows})
	require.NoError(t, err)
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: string(corev1.Windows), Namespace: testNamespace}, Spec: workspacesv1alpha1.WorkspaceSpec{OS: corev1.Windows, Mounts: []workspacesv1alpha1.WorkspaceMount{{Name: testWindowsMount, Path: testWindowsMount, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name}}}}}
	resolved, reason, _, err := reconciler.resolveWorkspaceDependencies(context.Background(), workspace, &resolvedWorkspace{runtime: platform, image: "custom/windows"})
	require.NoError(t, err)
	require.Empty(t, reason)
	require.Len(t, resolved.initializers, 1)
	assert.Equal(t, "custom/windows", resolved.initializers[0].image)
	assert.Empty(t, resolved.initializers[0].action.Command)
	assert.Contains(t, resolved.initializers[0].action.Script, "$LASTEXITCODE")
	assert.Equal(t, `C:\workspace\code`, resolved.initializers[0].action.WorkingDirectory)
	_, err = workspaceRuntimePod(workspace, resolved)
	require.NoError(t, err, "a custom Windows image must not require a Linux initializer image")
	workspace.Spec.Mounts[0].ReadOnly = true
	_, reason, _, err = reconciler.resolveWorkspaceDependencies(context.Background(), workspace, &resolvedWorkspace{runtime: platform})
	require.NoError(t, err)
	assert.Equal(t, "WorktreeNotReady", reason)
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
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "electron"}, Spec: workspacesv1alpha1.WorkspaceExecSpec{Command: []string{"electron.exe", "main.cjs"}}}
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: corev1.Windows})
	require.NoError(t, err)
	target := &resolvedProcessTarget{platform: platform, runtime: processruntime.Target{Executable: testWindowsExecutable, Endpoint: testWindowsEndpoint}, defaultDirectory: rcplatform.WorkspaceDirectory}
	request, err := (&WorkspaceExecReconciler{}).processStartRequest(context.Background(), process, target)
	require.NoError(t, err)
	assert.Empty(t, request.WorkingDirectory)
	assert.Equal(t, processruntime.DefaultDirectoryWorkspace, request.DefaultDirectory)
	assert.Empty(t, request.RuntimeDirectory)
	assert.Empty(t, request.TranscriptPath)
}
