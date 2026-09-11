package rcplatform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/nekomeowww/rc/internal/lifecycle"
)

const (
	testWindowsImage = "example/windows"
	testProcessName  = "electron"
)

func TestResolveOwnsOSPlacement(t *testing.T) {
	t.Parallel()
	selector := map[string]string{"pool": "desktop"}
	runtime, err := Resolve(Target{OS: corev1.Windows, Placement: Placement{NodeSelector: selector}})
	require.NoError(t, err)
	selector["pool"] = "changed"

	pod, err := runtime.TranscriptReaderPod(TranscriptPodIntent{
		Metadata: metav1.ObjectMeta{Name: "logs", Namespace: "development"},
		Image:    testWindowsImage, HomeClaim: "home", ProcessID: "codex-01",
	})
	require.NoError(t, err)
	assert.Equal(t, corev1.Windows, pod.Spec.OS.Name)
	assert.Equal(t, "windows", pod.Spec.NodeSelector[corev1.LabelOSStable])
	assert.Equal(t, "desktop", pod.Spec.NodeSelector["pool"])
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	assert.True(t, pod.Spec.Containers[0].VolumeMounts[0].ReadOnly)
	assert.Equal(t, []string{windowsExecutable, "transcript", "codex-01"}, pod.Spec.Containers[0].Command)
}

func TestResolveRejectsPlacementConflict(t *testing.T) {
	t.Parallel()
	_, err := Resolve(Target{OS: corev1.Windows, Placement: Placement{NodeSelector: map[string]string{corev1.LabelOSStable: "linux"}}})
	require.ErrorIs(t, err, ErrPlacementConflict)
}

func TestWindowsWorkspacePodIsRenderedOnce(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Windows})
	require.NoError(t, err)
	action := lifecycle.Action{Script: `New-Item -ItemType Directory C:\workspace\app`}
	pod, err := runtime.WorkspacePod(WorkspacePodIntent{
		Metadata: metav1.ObjectMeta{Name: testProcessName}, Image: testWindowsImage, HomeClaim: testProcessName,
		Initializers: []Initializer{{Name: "checkout", Image: testWindowsImage, Action: action}},
	})
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Empty(t, pod.Spec.InitContainers)
	assert.Empty(t, container.Command)
	require.GreaterOrEqual(t, len(container.Args), 8)
	assert.Equal(t, []string{windowsExecutable, serveCommand, socketArgument, `\\.\pipe\rc-kube`, stateDirectoryArgument, `C:\home\agent\.rc\processes`, "--initialize-actions"}, container.Args[:7])
	encoded := container.Args[7]
	actions, err := lifecycle.Decode(encoded)
	require.NoError(t, err)
	assert.Equal(t, []lifecycle.Action{action}, actions)
	assert.Empty(t, container.Env, "process environment does not enter the supervisor")
}

func TestDarwinWorkspacePodUsesVMProviderContract(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: Darwin, Placement: Placement{Tolerations: []corev1.Toleration{{Key: "virtual-kubelet.io/provider", Value: "macos-vz", Operator: corev1.TolerationOpEqual, Effect: corev1.TaintEffectNoSchedule}}}})
	require.NoError(t, err)
	action := lifecycle.Action{Command: []string{"xcodebuild", "-version"}}
	pod, err := runtime.WorkspacePod(WorkspacePodIntent{
		Metadata: metav1.ObjectMeta{Name: "xcode", Namespace: "development"}, Image: "example/macos:26.3",
		HomeHostPath: "/Users/runner/.local/share/rc/development/xcode",
		Initializers: []Initializer{{Name: "initialize", Image: "example/macos:26.3", Action: action}},
	})
	require.NoError(t, err)
	assert.Nil(t, pod.Spec.OS, "the Kubernetes PodOS schema does not admit darwin")
	assert.Equal(t, "darwin", pod.Spec.NodeSelector[corev1.LabelOSStable])
	assert.Nil(t, pod.Spec.SecurityContext)
	assert.Empty(t, pod.Spec.InitContainers, "macOS-vz represents the first container as a VM")
	require.Len(t, pod.Spec.Volumes, 1)
	require.NotNil(t, pod.Spec.Volumes[0].HostPath)
	assert.Equal(t, "/Users/runner/.local/share/rc/development/xcode", pod.Spec.Volumes[0].HostPath.Path)
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Empty(t, container.Command, "macOS-vz does not execute the container command")
	assert.Nil(t, container.ReadinessProbe, "macOS-vz gates readiness on its postStart hook")
	assert.Nil(t, container.SecurityContext)
	assert.Equal(t, darwinHome, container.VolumeMounts[0].MountPath)
	assert.Equal(t, []corev1.EnvVar{{Name: darwinExecArgvEnv, Value: "1"}}, container.Env)
	require.NotNil(t, container.Lifecycle)
	require.NotNil(t, container.Lifecycle.PostStart)
	startup := container.Lifecycle.PostStart.Exec.Command
	require.Len(t, startup, 3)
	assert.Equal(t, []string{unixShellExecutable, "-c"}, startup[:2])
	assert.Contains(t, startup[2], `nohup "`+darwinExecutable+`" serve`)
	assert.Contains(t, startup[2], darwinHome+"/.rc/run/rc-kube.sock")

	target := runtime.ProcessTarget("development", "xcode", runtimeContainerName)
	assert.Equal(t, darwinExecutable, target.Executable)
	assert.Equal(t, darwinHome+"/.rc/run/rc-kube.sock", target.Endpoint)
}

func TestProcessCompilesLogicalRequest(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Windows})
	require.NoError(t, err)
	request := runtime.Process(ProcessIntent{
		ID: testProcessName, Command: []string{"electron.exe"}, AgentProfile: &AgentProfile{Type: "codex"}, ExposeCredentials: true,
	})
	assert.Empty(t, request.WorkingDirectory)
	assert.Equal(t, processruntime.DefaultDirectoryWorkspace, request.DefaultDirectory)
	assert.Equal(t, &processruntime.AgentRef{Type: "codex", Credential: "default"}, request.Agent)
	assert.True(t, request.ExposeCredentials)
	assert.Empty(t, request.RuntimeDirectory)
	assert.Empty(t, request.TranscriptPath)
	assert.Empty(t, request.SSHConfigPath)
	assert.NotContains(t, request.Environment, "CODEX_HOME")
	assert.NotContains(t, request.Environment, "RC_CREDENTIALS_DIR")
}
