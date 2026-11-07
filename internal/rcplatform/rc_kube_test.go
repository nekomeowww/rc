package rcplatform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nekomeowww/rc/internal/lifecycle"
)

func testRCKubeIntent() WorkspacePodIntent {
	action := lifecycle.Action{Script: "true"}
	return WorkspacePodIntent{
		Metadata:     metav1.ObjectMeta{Name: testProcessName},
		Image:        testWindowsImage,
		HomeClaim:    testProcessName,
		HomeHostPath: "/var/lib/rc/" + testProcessName,
		Initializers: []Initializer{
			{
				Name:   "prepare",
				Image:  "example/prepare",
				Action: action,
			},
		},
		BeforeStop:  []lifecycle.Action{action},
		RCKubeImage: testRCKubeImage,
	}
}

func findRCKubeMount(container corev1.Container) *corev1.VolumeMount {
	for index := range container.VolumeMounts {
		if container.VolumeMounts[index].Name == rcKubeVolumeName {
			return &container.VolumeMounts[index]
		}
	}

	return nil
}

func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for index := range pod.Spec.Volumes {
		if pod.Spec.Volumes[index].Name == name {
			return &pod.Spec.Volumes[index]
		}
	}

	return nil
}

func assertInstallsRCKube(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	require.NotEmpty(t, pod.Spec.InitContainers)
	install := pod.Spec.InitContainers[0]
	assert.Equal(t, RCKubeInstallContainerName, install.Name, "install rc-kube before every other init container")
	assert.Equal(t, testRCKubeImage, install.Image)
	assert.Equal(t, []string{"/usr/local/bin/rc-kube", "install", "/opt/rc/bin"}, install.Command)
	mount := findRCKubeMount(install)
	require.NotNil(t, mount)
	assert.Equal(t, "/opt/rc/bin", mount.MountPath)
	assert.False(t, mount.ReadOnly, "the install container writes the binaries")
	require.NotNil(t, install.SecurityContext)
	require.NotNil(t, install.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *install.SecurityContext.AllowPrivilegeEscalation)

	volume := findVolume(pod, rcKubeVolumeName)
	require.NotNil(t, volume)
	assert.NotNil(t, volume.EmptyDir, "the binaries live only as long as the Pod")
}

func TestLinuxWorkspacePodRunsInjectedRCKube(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	runtime, err := Resolve(Target{OS: corev1.Linux})
	requirements.NoError(err)

	pod, err := runtime.WorkspacePod(testRCKubeIntent())
	requirements.NoError(err)

	assertInstallsRCKube(t, pod)
	requirements.Len(pod.Spec.InitContainers, 2)
	initializer := pod.Spec.InitContainers[1]
	assertions.Equal("example/prepare", initializer.Image, "an initializer keeps its own image")
	assertions.Equal(RCKubeExecutable, initializer.Command[0], "an initializer runs the injected rc-kube")

	requirements.Len(pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assertions.Equal(testWindowsImage, container.Image, "the runtime keeps the Workspace image")
	assertions.Equal([]string{"/opt/rc/bin/rc-kube", serveCommand}, container.Command)
	assertions.Equal([]string{socketArgument, "/run/rc/rc-kube.sock"}, container.ReadinessProbe.Exec.Command[2:], "keep the socket path")
	assertions.Equal(RCKubeExecutable, container.ReadinessProbe.Exec.Command[0])
	assertions.Equal(RCKubeExecutable, container.Lifecycle.PreStop.Exec.Command[0])
	assertions.Contains(container.Args, "/home/agent/.rc/processes", "keep the state directory")

	for _, checked := range []corev1.Container{initializer, container} {
		mount := findRCKubeMount(checked)
		requirements.NotNil(mount, "%s mounts the injected binaries", checked.Name)
		assertions.Equal(RCKubeDirectory, mount.MountPath)
		assertions.True(mount.ReadOnly, "%s cannot change the injected binaries", checked.Name)
	}

	target := runtime.ProcessTarget("development", pod.Name, runtimeContainerName)
	assertions.Equal(RCKubeExecutable, target.Executable, "Pod exec runs the injected rc-kube")
	assertions.Equal("/run/rc/rc-kube.sock", target.Endpoint)
}

func TestLinuxRuntimePodsRequireRCKubeImage(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)
	intent := testRCKubeIntent()
	intent.RCKubeImage = ""

	_, err = runtime.WorkspacePod(intent)
	require.ErrorContains(t, err, "rc-kube image")
	_, err = runtime.EnvironmentEditorPod(EditorPodIntent{
		Metadata:  metav1.ObjectMeta{Name: testProcessName},
		Image:     testWindowsImage,
		HomeClaim: testProcessName,
	})
	require.ErrorContains(t, err, "rc-kube image")
}

func TestNonLinuxWorkspacePodsKeepImageRCKube(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		os         corev1.OSName
		executable string
	}{
		{
			os:         corev1.Windows,
			executable: windowsExecutable,
		},
		{
			os:         Darwin,
			executable: darwinExecutable,
		},
	} {
		t.Run(string(test.os), func(t *testing.T) {
			t.Parallel()
			runtime, err := Resolve(Target{OS: test.os})
			require.NoError(t, err)
			intent := testRCKubeIntent()
			intent.RCKubeImage = ""

			pod, err := runtime.WorkspacePod(intent)
			require.NoError(t, err, "only Linux needs the rc-kube image")
			assert.Nil(t, findVolume(pod, rcKubeVolumeName))
			for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
				assert.NotEqual(t, RCKubeInstallContainerName, container.Name)
				assert.Nil(t, findRCKubeMount(container))
			}
			assert.Equal(t, test.executable, runtime.ProcessTarget("development", pod.Name, runtimeContainerName).Executable)
		})
	}
}

func TestLinuxEditorPodRunsInjectedRCKube(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	runtime, err := Resolve(Target{OS: corev1.Linux})
	requirements.NoError(err)

	pod, err := runtime.EnvironmentEditorPod(EditorPodIntent{
		Metadata:    metav1.ObjectMeta{Name: testProcessName},
		Image:       testWindowsImage,
		HomeClaim:   testProcessName,
		RCKubeImage: testRCKubeImage,
	})
	requirements.NoError(err)

	assertInstallsRCKube(t, pod)
	requirements.Len(pod.Spec.InitContainers, 2)
	assertions.Equal(sudoersContainerName, pod.Spec.InitContainers[1].Name, "the sudoers step still uses the image shell")
	container := pod.Spec.Containers[0]
	assertions.Equal(RCKubeExecutable, container.Command[0])
	assertions.Equal(RCKubeExecutable, container.ReadinessProbe.Exec.Command[0])
	mount := findRCKubeMount(container)
	requirements.NotNil(mount)
	assertions.True(mount.ReadOnly)
}

func TestFromPodFindsTheRCKubeOfThePod(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)
	injected, err := runtime.WorkspacePod(testRCKubeIntent())
	require.NoError(t, err)

	restored, err := FromPod(injected)
	require.NoError(t, err)
	assert.Equal(t, RCKubeExecutable, restored.ProcessTarget("development", injected.Name, runtimeContainerName).Executable)

	// A Pod that an older controller created runs rc-kube from the image PATH.
	legacy := injected.DeepCopy()
	legacy.Spec.InitContainers = legacy.Spec.InitContainers[1:]
	legacy.Spec.Containers[0].Command = []string{"rc-kube", serveCommand}
	restored, err = FromPod(legacy)
	require.NoError(t, err)
	assert.Equal(t, "rc-kube", restored.ProcessTarget("development", legacy.Name, runtimeContainerName).Executable, "keep using the rc-kube of a running older Pod")
}

func TestLinuxTranscriptReaderUsesImageRCKube(t *testing.T) {
	t.Parallel()
	runtime, err := Resolve(Target{OS: corev1.Linux})
	require.NoError(t, err)

	pod, err := runtime.TranscriptReaderPod(TranscriptPodIntent{
		Metadata:  metav1.ObjectMeta{Name: "logs"},
		Image:     testWindowsImage,
		HomeClaim: "home",
		ProcessID: testProcessName,
	})
	require.NoError(t, err)
	assert.Empty(t, pod.Spec.InitContainers)
	assert.Equal(t, []string{"rc-kube", transcriptCommand, testProcessName}, pod.Spec.Containers[0].Command)
}
