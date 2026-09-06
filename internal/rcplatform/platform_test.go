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
