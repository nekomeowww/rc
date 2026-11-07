package rcplatform

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/lifecycle"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
)

const (
	homeVolumeName       = "home"
	runtimeVolumeName    = "rc-runtime"
	sudoersContainerName = "configure-sudo"
	sudoersVolumeName    = "sudoers"
	sudoersMountPath     = "/etc/sudoers.d"
	sudoersFilePath      = sudoersMountPath + "/agent"
	sudoersRule          = "agent ALL=(ALL:ALL) NOPASSWD:ALL"
	darwinExecArgvEnv    = "MACOS_VZ_KUBERNETES_EXEC_ARGV"
	unixShellExecutable  = "/bin/sh"
	actionsArgument      = "--actions"
	lifecycleCommand     = "lifecycle"
	servicesArgument     = "--services"
	// rcKubeVolumeName is the emptyDir that holds the injected rc-kube.
	rcKubeVolumeName = "rc-kube-bin"
	// RCKubeInstallContainerName is the first init container of a Linux
	// runtime Pod. It copies rc-kube and tini into RCKubeDirectory.
	RCKubeInstallContainerName = "rc-kube-install"
	// rcKubeImageExecutable is rc-kube in the rc-kube image. The image keeps
	// tini in the same directory.
	rcKubeImageExecutable = "/usr/local/bin/rc-kube"
	rcKubeInstallCommand  = "install"
	transcriptCommand     = "transcript"
)

// rcKubeInjection returns what a Linux Pod needs to run the rc-kube of the
// controller instead of the one in its image: an init container that copies
// rc-kube and tini from the rc-kube image, the emptyDir it copies them to,
// and the read-only mount for the containers that run them.
func rcKubeInjection(image string) (corev1.Container, corev1.Volume, corev1.VolumeMount, error) {
	if image == "" {
		return corev1.Container{}, corev1.Volume{}, corev1.VolumeMount{}, fmt.Errorf("linux runtime Pods require an rc-kube image")
	}
	install := corev1.Container{
		Name:  RCKubeInstallContainerName,
		Image: image,
		Command: []string{
			rcKubeImageExecutable,
			rcKubeInstallCommand,
			RCKubeDirectory,
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      rcKubeVolumeName,
				MountPath: RCKubeDirectory,
			},
		},
		SecurityContext: runtimepolicy.ContainerSecurityContext(corev1.Linux),
	}
	volume := corev1.Volume{
		Name:         rcKubeVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	mount := corev1.VolumeMount{
		Name:      rcKubeVolumeName,
		MountPath: RCKubeDirectory,
		ReadOnly:  true,
	}

	return install, volume, mount, nil
}

// Initializer runs one lifecycle action before the runtime becomes ready.
type Initializer struct {
	Name, Image string
	Action      lifecycle.Action
}

// WorkspacePodIntent describes a complete long-lived Workspace runtime.
type WorkspacePodIntent struct {
	Metadata          metav1.ObjectMeta
	Image, HomeClaim  string
	HomeHostPath      string
	TranscriptScope   string
	HotMountRoot      string
	ServiceAccount    string
	AutomountToken    bool
	Resources         corev1.ResourceRequirements
	AdditionalVolumes []corev1.Volume
	AdditionalMounts  []corev1.VolumeMount
	Initializers      []Initializer
	BeforeStop        []lifecycle.Action
	// PriorityClassName is set only on the long-lived Workspace runtime Pod.
	PriorityClassName string
	// Services reach rc-kube serve as one argument. ServiceEnvironment holds
	// the runtime container variables their Secret and ConfigMap values come
	// from; both are part of the Pod, so a change takes a new runtime.
	Services           []processruntime.Service
	ServiceEnvironment []corev1.EnvVar
	// LXCFSDir is the host directory where LXCFS serves its files. When set,
	// a Linux runtime bind-mounts the LXCFS views over the matching /proc and
	// /sys files. Empty adds nothing.
	LXCFSDir string
	// RCKubeImage provides the rc-kube and tini that a Linux runtime runs.
	// It is required on Linux and not used on other operating systems.
	RCKubeImage string
}

// LXCFSVolumePrefix starts the name of every LXCFS volume in a Workspace Pod.
const LXCFSVolumePrefix = "lxcfs-"

// lxcfsFiles are the files LXCFS virtualizes, as container paths. Each one is
// served at the same path under the LXCFS directory on the host.
var lxcfsFiles = []string{
	"/proc/cpuinfo",
	"/proc/meminfo",
	"/proc/stat",
	"/proc/uptime",
	"/proc/loadavg",
	"/proc/diskstats",
	"/proc/swaps",
	"/sys/devices/system/cpu/online",
}

// lxcfsVolumes returns read-only bind mounts of the LXCFS files in dir. The
// hostPath type is File, so a node without LXCFS fails the mount instead of
// mounting an empty path.
func lxcfsVolumes(dir string) ([]corev1.Volume, []corev1.VolumeMount) {
	if dir == "" {
		return nil, nil
	}
	kind := corev1.HostPathFile
	volumes := make([]corev1.Volume, 0, len(lxcfsFiles))
	mounts := make([]corev1.VolumeMount, 0, len(lxcfsFiles))
	for _, file := range lxcfsFiles {
		name := LXCFSVolumePrefix + strings.ReplaceAll(strings.TrimPrefix(file, "/"), "/", "-")
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: path.Join(dir, file),
				Type: &kind,
			}},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      name,
			MountPath: file,
			ReadOnly:  true,
		})
	}

	return volumes, mounts
}

// WorkspacePod compiles a final Pod without post-construction platform rewrites.
func (runtime Runtime) WorkspacePod(intent WorkspacePodIntent) (*corev1.Pod, error) {
	if intent.LXCFSDir != "" && runtime.os != corev1.Linux {
		return nil, fmt.Errorf("LXCFS mounts require a Linux runtime")
	}
	if runtime.os == Darwin {
		if len(intent.Services) > 0 {
			return nil, fmt.Errorf("darwin Workspace runtimes do not support services")
		}
		return runtime.darwinWorkspacePod(intent)
	}
	volumes := make([]corev1.Volume, 0, len(intent.AdditionalVolumes)+4)
	volumes = append(volumes, corev1.Volume{Name: homeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: intent.HomeClaim}}})
	if intent.HotMountRoot != "" {
		if runtime.os != corev1.Linux {
			return nil, fmt.Errorf("hot Worktree mounts require a Linux runtime")
		}
		kind := corev1.HostPathDirectoryOrCreate
		volumes = append(volumes,
			corev1.Volume{Name: "hot-worktrees", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: intent.HotMountRoot + "/visible", Type: &kind}}},
			corev1.Volume{Name: "hot-worktree-roots", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: intent.HotMountRoot + "/roots", Type: &kind}}},
		)
	}
	lxcfsVolumeList, lxcfsMounts := lxcfsVolumes(intent.LXCFSDir)
	volumes = append(volumes, slices.Clone(intent.AdditionalVolumes)...)
	volumes = append(volumes, lxcfsVolumeList...)
	volumes = append(volumes, corev1.Volume{Name: runtimeVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	var initContainers []corev1.Container
	var rcKubeMounts []corev1.VolumeMount
	if runtime.os == corev1.Linux {
		install, volume, mount, err := rcKubeInjection(intent.RCKubeImage)
		if err != nil {
			return nil, err
		}
		initContainers = append(initContainers, install)
		volumes = append(volumes, volume)
		rcKubeMounts = append(rcKubeMounts, mount)
	}
	mounts := make([]corev1.VolumeMount, 0, len(intent.AdditionalMounts)+4)
	mounts = append(mounts, corev1.VolumeMount{Name: homeVolumeName, MountPath: runtime.layout.home})
	if intent.HotMountRoot != "" {
		propagation := corev1.MountPropagationHostToContainer
		mounts = append(mounts,
			corev1.VolumeMount{Name: "hot-worktrees", MountPath: runtime.MountPath("/workspace"), MountPropagation: &propagation},
			corev1.VolumeMount{Name: "hot-worktree-roots", MountPath: runtime.MountPath("/mnt/rc/worktrees"), MountPropagation: &propagation},
		)
	}
	mounts = append(mounts, slices.Clone(intent.AdditionalMounts)...)
	mounts = append(mounts, lxcfsMounts...)
	mounts = append(mounts, corev1.VolumeMount{Name: runtimeVolumeName, MountPath: runtime.layout.run})
	mounts = append(mounts, rcKubeMounts...)

	// A Linux runtime without a Service Account token hides the API server
	// variables from every process that rc-kube starts. With a token, the
	// variables stay, so in-cluster clients keep working.
	var runtimeFlags []string
	if runtime.os == corev1.Linux && !intent.AutomountToken {
		runtimeFlags = []string{"--" + processruntime.HideKubernetesEnvironmentFlag}
	}
	lifecycleCommandLine := func(encoded string) []string {
		command := []string{runtime.layout.executable, lifecycleCommand}
		command = append(command, runtimeFlags...)
		return append(command, actionsArgument, encoded)
	}

	for _, initializer := range intent.Initializers {
		if initializer.Image == "" {
			return nil, fmt.Errorf("workspace initializer %s has no runtime image", initializer.Name)
		}
		encoded, err := lifecycle.Encode([]lifecycle.Action{initializer.Action})
		if err != nil {
			return nil, fmt.Errorf("encode Workspace initializer %s: %w", initializer.Name, err)
		}
		initContainers = append(initContainers, corev1.Container{
			Name: initializer.Name, Image: initializer.Image,
			Command:      lifecycleCommandLine(encoded),
			VolumeMounts: slices.Clone(mounts), SecurityContext: runtimepolicy.ContainerSecurityContext(runtime.os),
		})
	}

	var containerLifecycle *corev1.Lifecycle
	if len(intent.BeforeStop) > 0 {
		encoded, err := lifecycle.Encode(intent.BeforeStop)
		if err != nil {
			return nil, fmt.Errorf("encode Workspace beforeStop actions: %w", err)
		}
		containerLifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{
			Command: lifecycleCommandLine(encoded),
		}}}
	}

	serveArgs := []string{socketArgument, runtime.layout.endpoint, stateDirectoryArgument, runtime.join(runtime.layout.home, ".rc", "processes")}
	serveArgs = append(serveArgs, runtimeFlags...)
	if intent.TranscriptScope != "" {
		serveArgs = append(serveArgs, "--transcript-scope", intent.TranscriptScope)
	}
	if len(intent.Services) > 0 {
		encoded, err := processruntime.EncodeServices(intent.Services)
		if err != nil {
			return nil, fmt.Errorf("encode Workspace services: %w", err)
		}
		serveArgs = append(serveArgs, servicesArgument, encoded)
	}
	container := corev1.Container{
		Name: runtimeContainerName, Image: intent.Image,
		Command: []string{runtime.layout.executable, serveCommand}, Args: serveArgs,
		Env:       slices.Clone(intent.ServiceEnvironment),
		Resources: intent.Resources, VolumeMounts: mounts, Lifecycle: containerLifecycle,
		ReadinessProbe: runtime.readinessProbe(), SecurityContext: runtimepolicy.ContainerSecurityContext(runtime.os),
	}
	if runtime.os == corev1.Windows {
		actions := make([]lifecycle.Action, 0, len(intent.Initializers))
		for _, initializer := range intent.Initializers {
			actions = append(actions, initializer.Action)
		}
		if len(actions) > 0 {
			encoded, err := lifecycle.Encode(actions)
			if err != nil {
				return nil, fmt.Errorf("encode Windows Workspace initializers: %w", err)
			}
			container.Args = append(container.Command, container.Args...)
			container.Args = append(container.Args, "--initialize-actions", encoded)
		} else {
			container.Args = append(container.Command, container.Args...)
		}
		container.Command = nil
		initContainers = nil
	}

	pod := &corev1.Pod{
		ObjectMeta: *intent.Metadata.DeepCopy(),
		Spec: runtime.podSpec(intent.ServiceAccount, &intent.AutomountToken, corev1.RestartPolicyAlways,
			initContainers, []corev1.Container{container}, volumes),
	}
	pod.Spec.PriorityClassName = intent.PriorityClassName
	if runtime.os == corev1.Linux {
		// Do not add the other Services of the namespace to the environment
		// of Workspace processes.
		enableServiceLinks := false
		pod.Spec.EnableServiceLinks = &enableServiceLinks
	}

	return pod, nil
}

func (runtime Runtime) darwinWorkspacePod(intent WorkspacePodIntent) (*corev1.Pod, error) {
	if intent.HomeHostPath == "" {
		return nil, fmt.Errorf("darwin Workspace home host path is required")
	}
	hostPathType := corev1.HostPathDirectoryOrCreate
	volumes := []corev1.Volume{{Name: homeVolumeName, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
		Path: intent.HomeHostPath, Type: &hostPathType,
	}}}}
	mounts := []corev1.VolumeMount{{Name: homeVolumeName, MountPath: runtime.layout.home}}
	actions := make([]lifecycle.Action, 0, len(intent.Initializers))
	for _, initializer := range intent.Initializers {
		actions = append(actions, initializer.Action)
	}
	encoded, err := lifecycle.Encode(actions)
	if err != nil {
		return nil, fmt.Errorf("encode Darwin Workspace initializers: %w", err)
	}
	stateDirectory := runtime.join(runtime.layout.home, ".rc", "processes")
	logPath := runtime.join(runtime.layout.home, ".rc", "rc-kube.log")
	startup := fmt.Sprintf("set -eu\nmkdir -p %q %q\n", runtime.layout.run, stateDirectory)
	if len(actions) > 0 {
		startup += fmt.Sprintf("%q %s %s %q\n", runtime.layout.executable, lifecycleCommand, actionsArgument, encoded)
	}
	startup += fmt.Sprintf("if ! %[1]q health %[2]s %[3]q >/dev/null 2>&1; then\n  nohup %[1]q %[4]s %[2]s %[3]q %[5]s %[6]q >%[7]q 2>&1 </dev/null &\nfi\nattempt=0\nuntil %[1]q health %[2]s %[3]q >/dev/null 2>&1; do\n  attempt=$((attempt + 1))\n  [ \"$attempt\" -lt 8 ] || exit 1\n  sleep 1\ndone\n", runtime.layout.executable, socketArgument, runtime.layout.endpoint, serveCommand, stateDirectoryArgument, stateDirectory, logPath)

	containerLifecycle := &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{
		Command: []string{unixShellExecutable, "-c", startup},
	}}}
	if len(intent.BeforeStop) > 0 {
		beforeStop, encodeErr := lifecycle.Encode(intent.BeforeStop)
		if encodeErr != nil {
			return nil, fmt.Errorf("encode Darwin Workspace beforeStop actions: %w", encodeErr)
		}
		containerLifecycle.PreStop = &corev1.LifecycleHandler{Exec: &corev1.ExecAction{
			Command: []string{runtime.layout.executable, lifecycleCommand, actionsArgument, beforeStop},
		}}
	}
	container := corev1.Container{
		Name: runtimeContainerName, Image: intent.Image, Resources: intent.Resources,
		Env: []corev1.EnvVar{{Name: darwinExecArgvEnv, Value: "1"}}, VolumeMounts: mounts, Lifecycle: containerLifecycle,
	}
	pod := &corev1.Pod{ObjectMeta: *intent.Metadata.DeepCopy(), Spec: runtime.podSpec(
		intent.ServiceAccount, &intent.AutomountToken, corev1.RestartPolicyAlways, nil,
		[]corev1.Container{container}, volumes,
	)}
	pod.Spec.PriorityClassName = intent.PriorityClassName

	return pod, nil
}

// EditorPodIntent describes the mutable WorkspaceEnvironment editor runtime.
type EditorPodIntent struct {
	TranscriptScope string
	Metadata        metav1.ObjectMeta
	Image           string
	HomeClaim       string
	ServiceAccount  string
	// RCKubeImage has the same role as in WorkspacePodIntent.
	RCKubeImage string
}

// EnvironmentEditorPod compiles a final editor Pod for the target OS.
func (runtime Runtime) EnvironmentEditorPod(intent EditorPodIntent) (*corev1.Pod, error) {
	automount := true
	volumes := []corev1.Volume{
		{Name: homeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: intent.HomeClaim}}},
		{Name: runtimeVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{{Name: homeVolumeName, MountPath: runtime.layout.home}, {Name: runtimeVolumeName, MountPath: runtime.layout.run}}
	container := corev1.Container{
		Name: runtimeContainerName, Image: intent.Image,
		Command:        []string{runtime.layout.executable, serveCommand},
		Args:           []string{socketArgument, runtime.layout.endpoint, stateDirectoryArgument, runtime.join(runtime.layout.home, ".rc", "processes")},
		ReadinessProbe: runtime.readinessProbe(), VolumeMounts: mounts,
		SecurityContext: runtimepolicy.ContainerSecurityContext(runtime.os),
	}
	if intent.TranscriptScope != "" {
		container.Args = append(container.Args, "--transcript-scope", intent.TranscriptScope)
	}
	var initContainers []corev1.Container
	if runtime.os == corev1.Windows {
		container.Args = append(container.Command, container.Args...)
		container.Command = nil
	} else {
		runAsRoot, runAsRootGroup := int64(0), int64(0)
		runAsNonRoot, disallowPrivilegeEscalation, allowPrivilegeEscalation := false, false, true
		container.SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation}
		install, volume, mount, err := rcKubeInjection(intent.RCKubeImage)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volume)
		container.VolumeMounts = append(container.VolumeMounts, mount)
		initContainers = []corev1.Container{install, {
			Name: sudoersContainerName, Image: intent.Image, Command: []string{"/bin/sh", "-ec"},
			Args: []string{fmt.Sprintf("chgrp 0 %[1]s && chmod 0755 %[1]s && printf '%%s\\n' %[2]q > %[3]s && chgrp 0 %[3]s && chmod 0440 %[3]s", sudoersMountPath, sudoersRule, sudoersFilePath)},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot: &runAsNonRoot, RunAsUser: &runAsRoot, RunAsGroup: &runAsRootGroup,
				AllowPrivilegeEscalation: &disallowPrivilegeEscalation,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: sudoersVolumeName, MountPath: sudoersMountPath}},
		}}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: sudoersVolumeName, MountPath: sudoersMountPath, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: sudoersVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}

	return &corev1.Pod{
		ObjectMeta: *intent.Metadata.DeepCopy(),
		Spec: runtime.podSpec(intent.ServiceAccount, &automount, corev1.RestartPolicyAlways,
			initContainers, []corev1.Container{container}, volumes),
	}, nil
}

// TranscriptPodIntent describes a one-shot retained transcript reader.
type TranscriptPodIntent struct {
	Metadata                    metav1.ObjectMeta
	Image, HomeClaim, ProcessID string
}

// TranscriptReaderPod compiles a placement-correct, read-only helper Pod.
func (runtime Runtime) TranscriptReaderPod(intent TranscriptPodIntent) (*corev1.Pod, error) {
	if intent.ProcessID == "" {
		return nil, fmt.Errorf("transcript process ID is required")
	}
	automount := false
	container := corev1.Container{
		Name: "reader", Image: intent.Image,
		Command:         []string{runtime.imageExecutable(), transcriptCommand, intent.ProcessID},
		VolumeMounts:    []corev1.VolumeMount{{Name: homeVolumeName, MountPath: runtime.layout.home, ReadOnly: true}},
		SecurityContext: runtimepolicy.ContainerSecurityContext(runtime.os),
	}
	volumes := []corev1.Volume{{Name: homeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: intent.HomeClaim, ReadOnly: true}}}}
	return &corev1.Pod{
		ObjectMeta: *intent.Metadata.DeepCopy(),
		Spec:       runtime.podSpec("", &automount, corev1.RestartPolicyNever, nil, []corev1.Container{container}, volumes),
	}, nil
}

// imageExecutable is the rc-kube of the Pod image. The transcript reader runs
// it because rcctl, which creates the reader, does not know the rc-kube image
// of the controller.
func (runtime Runtime) imageExecutable() string {
	if runtime.os == corev1.Linux {
		return imageExecutable
	}

	return runtime.layout.executable
}

func (runtime Runtime) readinessProbe() *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
		Command: []string{runtime.layout.executable, "health", socketArgument, runtime.layout.endpoint},
	}}, PeriodSeconds: 5, TimeoutSeconds: 3}
}

func (runtime Runtime) podSpec(serviceAccount string, automount *bool, restart corev1.RestartPolicy, init []corev1.Container, containers []corev1.Container, volumes []corev1.Volume) corev1.PodSpec {
	spec := corev1.PodSpec{
		ServiceAccountName: serviceAccount, AutomountServiceAccountToken: automount,
		RestartPolicy: restart,
		NodeSelector:  mapsClone(runtime.placement.NodeSelector), Tolerations: slices.Clone(runtime.placement.Tolerations),
		Affinity: affinityClone(runtime.placement.Affinity), RuntimeClassName: cloneStringPointer(runtime.placement.RuntimeClassName),
		SecurityContext: runtimepolicy.PodSecurityContext(runtime.os), InitContainers: init, Containers: containers, Volumes: volumes,
	}
	if runtime.os != Darwin {
		spec.OS = &corev1.PodOS{Name: runtime.os}
	}
	return spec
}

func mapsClone(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	maps.Copy(output, input)
	return output
}

func affinityClone(input *corev1.Affinity) *corev1.Affinity {
	if input == nil {
		return nil
	}
	return input.DeepCopy()
}
