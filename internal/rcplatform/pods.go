package rcplatform

import (
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
)

// Initializer runs one lifecycle action before the runtime becomes ready.
type Initializer struct {
	Name, Image string
	Action      lifecycle.Action
}

// WorkspacePodIntent describes a complete long-lived Workspace runtime.
type WorkspacePodIntent struct {
	Metadata          metav1.ObjectMeta
	Image, HomeClaim  string
	ServiceAccount    string
	AutomountToken    bool
	Resources         corev1.ResourceRequirements
	AdditionalVolumes []corev1.Volume
	AdditionalMounts  []corev1.VolumeMount
	Initializers      []Initializer
	BeforeStop        []lifecycle.Action
}

// WorkspacePod compiles a final Pod without post-construction platform rewrites.
func (runtime Runtime) WorkspacePod(intent WorkspacePodIntent) (*corev1.Pod, error) {
	volumes := make([]corev1.Volume, 0, len(intent.AdditionalVolumes)+2)
	volumes = append(volumes, corev1.Volume{Name: homeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: intent.HomeClaim}}})
	volumes = append(volumes, slices.Clone(intent.AdditionalVolumes)...)
	volumes = append(volumes, corev1.Volume{Name: runtimeVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	mounts := make([]corev1.VolumeMount, 0, len(intent.AdditionalMounts)+2)
	mounts = append(mounts, corev1.VolumeMount{Name: homeVolumeName, MountPath: runtime.layout.home})
	mounts = append(mounts, slices.Clone(intent.AdditionalMounts)...)
	mounts = append(mounts, corev1.VolumeMount{Name: runtimeVolumeName, MountPath: runtime.layout.run})

	initContainers := make([]corev1.Container, 0, len(intent.Initializers))
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
			Command:      []string{runtime.layout.executable, "lifecycle", "--actions", encoded},
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
			Command: []string{runtime.layout.executable, "lifecycle", "--actions", encoded},
		}}}
	}

	serveArgs := []string{socketArgument, runtime.layout.endpoint, stateDirectoryArgument, runtime.join(runtime.layout.home, ".rc", "processes")}
	container := corev1.Container{
		Name: runtimeContainerName, Image: intent.Image,
		Command: []string{runtime.layout.executable, serveCommand}, Args: serveArgs,
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

	return &corev1.Pod{
		ObjectMeta: *intent.Metadata.DeepCopy(),
		Spec: runtime.podSpec(intent.ServiceAccount, &intent.AutomountToken, corev1.RestartPolicyAlways,
			initContainers, []corev1.Container{container}, volumes),
	}, nil
}

// EditorPodIntent describes the mutable WorkspaceEnvironment editor runtime.
type EditorPodIntent struct {
	Metadata       metav1.ObjectMeta
	Image          string
	HomeClaim      string
	ServiceAccount string
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
	var initContainers []corev1.Container
	if runtime.os == corev1.Windows {
		container.Args = append(container.Command, container.Args...)
		container.Command = nil
	} else {
		runAsRoot, runAsRootGroup := int64(0), int64(0)
		runAsNonRoot, disallowPrivilegeEscalation, allowPrivilegeEscalation := false, false, true
		container.SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation}
		initContainers = []corev1.Container{{
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
		Command:         []string{runtime.layout.executable, "transcript", intent.ProcessID},
		VolumeMounts:    []corev1.VolumeMount{{Name: homeVolumeName, MountPath: runtime.layout.home, ReadOnly: true}},
		SecurityContext: runtimepolicy.ContainerSecurityContext(runtime.os),
	}
	volumes := []corev1.Volume{{Name: homeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: intent.HomeClaim, ReadOnly: true}}}}
	return &corev1.Pod{
		ObjectMeta: *intent.Metadata.DeepCopy(),
		Spec:       runtime.podSpec("", &automount, corev1.RestartPolicyNever, nil, []corev1.Container{container}, volumes),
	}, nil
}

func (runtime Runtime) readinessProbe() *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
		Command: []string{runtime.layout.executable, "health", socketArgument, runtime.layout.endpoint},
	}}, PeriodSeconds: 5, TimeoutSeconds: 3}
}

func (runtime Runtime) podSpec(serviceAccount string, automount *bool, restart corev1.RestartPolicy, init []corev1.Container, containers []corev1.Container, volumes []corev1.Volume) corev1.PodSpec {
	return corev1.PodSpec{
		ServiceAccountName: serviceAccount, AutomountServiceAccountToken: automount,
		RestartPolicy: restart, OS: &corev1.PodOS{Name: runtime.os},
		NodeSelector: mapsClone(runtime.placement.NodeSelector), Tolerations: slices.Clone(runtime.placement.Tolerations),
		Affinity: affinityClone(runtime.placement.Affinity), RuntimeClassName: cloneStringPointer(runtime.placement.RuntimeClassName),
		SecurityContext: runtimepolicy.PodSecurityContext(runtime.os), InitContainers: init, Containers: containers, Volumes: volumes,
	}
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
