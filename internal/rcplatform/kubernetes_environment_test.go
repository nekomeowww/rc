package rcplatform

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/lifecycle"
)

const testHideKubernetesArgument = "--" + processruntime.HideKubernetesEnvironmentFlag

func testKubernetesEnvironmentIntent(automountToken bool) WorkspacePodIntent {
	action := lifecycle.Action{Script: "true"}
	return WorkspacePodIntent{
		Metadata:       metav1.ObjectMeta{Name: testProcessName},
		Image:          testWindowsImage,
		HomeClaim:      testProcessName,
		AutomountToken: automountToken,
		Initializers: []Initializer{
			{
				Name:   "checkout",
				Image:  testWindowsImage,
				Action: action,
			},
		},
		BeforeStop: []lifecycle.Action{action},
	}
}

// rcKubeCommandLines returns every rc-kube command line in a Workspace Pod:
// initializers, serve, and the beforeStop hook.
func rcKubeCommandLines(t *testing.T, pod *corev1.Pod) map[string][]string {
	t.Helper()
	require.Len(t, pod.Spec.InitContainers, 1)
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	require.NotNil(t, container.Lifecycle)
	require.NotNil(t, container.Lifecycle.PreStop)

	return map[string][]string{
		"initializer": pod.Spec.InitContainers[0].Command,
		"serve":       append(slices.Clone(container.Command), container.Args...),
		"beforeStop":  container.Lifecycle.PreStop.Exec.Command,
	}
}

func TestLinuxWorkspacePodDisablesServiceLinks(t *testing.T) {
	t.Parallel()
	for _, automountToken := range []bool{false, true} {
		runtime, err := Resolve(Target{OS: corev1.Linux})
		require.NoError(t, err)

		pod, err := runtime.WorkspacePod(testKubernetesEnvironmentIntent(automountToken))
		require.NoError(t, err)

		require.NotNil(t, pod.Spec.EnableServiceLinks, "set enableServiceLinks explicitly")
		assert.False(t, *pod.Spec.EnableServiceLinks, "do not inject other Services as variables")
	}
}

func TestLinuxWorkspacePodWithoutTokenHidesKubernetesEnvironment(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)

	pod, err := runtime.WorkspacePod(testKubernetesEnvironmentIntent(false))
	require.NoError(t, err)

	for name, command := range rcKubeCommandLines(t, pod) {
		assert.Contains(t, command, testHideKubernetesArgument, "%s hides the Kubernetes variables", name)
	}
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
}

func TestLinuxWorkspacePodWithTokenKeepsKubernetesEnvironment(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)

	pod, err := runtime.WorkspacePod(testKubernetesEnvironmentIntent(true))
	require.NoError(t, err)

	for name, command := range rcKubeCommandLines(t, pod) {
		assert.NotContains(t, command, testHideKubernetesArgument, "%s keeps the Kubernetes variables for in-cluster clients", name)
	}
}

func TestWindowsWorkspacePodKeepsServiceLinksAndEnvironment(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Windows})
	require.NoError(t, err)

	pod, err := runtime.WorkspacePod(testKubernetesEnvironmentIntent(false))
	require.NoError(t, err)

	assert.Nil(t, pod.Spec.EnableServiceLinks, "change only Linux runtime Pods")
	for _, container := range pod.Spec.Containers {
		assert.NotContains(t, container.Args, testHideKubernetesArgument, "change only Linux runtime Pods")
	}
}
