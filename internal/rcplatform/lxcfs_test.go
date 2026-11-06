package rcplatform

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testLXCFSDir = "/var/lib/lxcfs"

func testLXCFSIntent() WorkspacePodIntent {
	return WorkspacePodIntent{
		Metadata:     metav1.ObjectMeta{Name: testProcessName},
		Image:        testWindowsImage,
		HomeClaim:    testProcessName,
		HomeHostPath: "/var/lib/rc/" + testProcessName,
	}
}

func TestWorkspacePodWithoutLXCFSIsUnchanged(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	runtime, err := Resolve(Target{OS: corev1.Linux})
	requirements.NoError(err)

	pod, err := runtime.WorkspacePod(testLXCFSIntent())
	requirements.NoError(err)

	for _, volume := range pod.Spec.Volumes {
		assert.NotContains(t, volume.Name, "lxcfs", "add no LXCFS volume when the directory is empty")
	}
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			assert.NotContains(t, mount.Name, "lxcfs", "add no LXCFS mount when the directory is empty")
		}
	}
}

func TestLinuxWorkspacePodMountsLXCFSFiles(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	runtime, err := Resolve(Target{OS: corev1.Linux})
	requirements.NoError(err)
	intent := testLXCFSIntent()
	intent.LXCFSDir = testLXCFSDir

	pod, err := runtime.WorkspacePod(intent)
	requirements.NoError(err)

	fileType := corev1.HostPathFile
	expectedFiles := map[string]string{
		"lxcfs-proc-cpuinfo":                  "/proc/cpuinfo",
		"lxcfs-proc-meminfo":                  "/proc/meminfo",
		"lxcfs-proc-stat":                     "/proc/stat",
		"lxcfs-proc-uptime":                   "/proc/uptime",
		"lxcfs-proc-loadavg":                  "/proc/loadavg",
		"lxcfs-proc-diskstats":                "/proc/diskstats",
		"lxcfs-proc-swaps":                    "/proc/swaps",
		"lxcfs-sys-devices-system-cpu-online": "/sys/devices/system/cpu/online",
	}
	volumes := make(map[string]corev1.Volume)
	for _, volume := range pod.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	requirements.Len(pod.Spec.Containers, 1)
	mounts := make(map[string]corev1.VolumeMount)
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		mounts[mount.Name] = mount
	}
	lxcfsVolumeCount := 0
	for name := range volumes {
		if strings.HasPrefix(name, "lxcfs-") {
			lxcfsVolumeCount++
		}
	}
	assertions.Equal(len(expectedFiles), lxcfsVolumeCount, "mount exactly the LXCFS file list")
	for name, file := range expectedFiles {
		volume, ok := volumes[name]
		requirements.True(ok, "volume %s", name)
		assertions.Equal(&corev1.HostPathVolumeSource{
			Path: testLXCFSDir + file,
			Type: &fileType,
		}, volume.HostPath, "bind the LXCFS file and fail when it is missing")
		mount, ok := mounts[name]
		requirements.True(ok, "mount %s", name)
		assertions.Equal(corev1.VolumeMount{
			Name:      name,
			MountPath: file,
			ReadOnly:  true,
		}, mount, "mount the LXCFS file read-only over the kernel file")
	}
}

func TestNonLinuxWorkspacePodRejectsLXCFS(t *testing.T) {
	t.Parallel()
	for _, osName := range []corev1.OSName{
		corev1.Windows,
		Darwin,
	} {
		t.Run(string(osName), func(t *testing.T) {
			t.Parallel()
			runtime, err := Resolve(Target{OS: osName})
			require.NoError(t, err)
			intent := testLXCFSIntent()
			intent.LXCFSDir = testLXCFSDir

			_, err = runtime.WorkspacePod(intent)
			assert.Error(t, err, "LXCFS applies only to Linux runtimes")
		})
	}
}
