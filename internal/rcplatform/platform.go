// Package rcplatform compiles logical rc runtime intents into target-platform
// Kubernetes objects and Pod-exec requests. It never inspects the manager's OS.
package rcplatform

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const (
	runtimeContainerName   = "rc-kube"
	windowsExecutable      = "rc-kube.exe"
	darwinExecutable       = "/usr/local/bin/rc-kube"
	darwinHome             = "/Volumes/My Shared Files/home"
	socketArgument         = "--socket"
	stateDirectoryArgument = "--state-dir"
	serveCommand           = "serve"
)

// Darwin is the node OS label used by macOS virtual-kubelet providers. The
// Kubernetes PodOS API itself currently exposes only Linux and Windows, so
// Darwin Pods are selected by node label and intentionally omit spec.os.
const Darwin corev1.OSName = "darwin"

// ErrUnsupportedOS reports an OS outside Kubernetes' Linux and Windows set.
var ErrUnsupportedOS = errors.New("unsupported runtime OS")

// ErrPlacementConflict reports an OS selector that contradicts the runtime OS.
var ErrPlacementConflict = errors.New("runtime OS conflicts with node selector")

// Placement contains the scheduling fields that must travel together with the
// runtime OS.
type Placement struct {
	NodeSelector     map[string]string
	Tolerations      []corev1.Toleration
	Affinity         *corev1.Affinity
	RuntimeClassName *string
}

// Target is the complete target-platform input understood by rcplatform.
type Target struct {
	OS        corev1.OSName
	Placement Placement
}

// Runtime is an immutable, validated target-platform compiler.
type Runtime struct {
	os        corev1.OSName
	layout    layout
	placement Placement
}

type layout struct {
	home, run            string
	executable, endpoint string
}

// Resolve validates and compiles an OS and its Kubernetes placement policy.
func Resolve(target Target) (Runtime, error) {
	osName := target.OS
	if osName == "" {
		osName = corev1.Linux
	}
	var targetLayout layout
	switch osName {
	case corev1.Linux:
		targetLayout = layout{home: "/home/agent", run: "/run/rc", executable: runtimeContainerName, endpoint: "/run/rc/rc-kube.sock"}
	case corev1.Windows:
		targetLayout = layout{home: `C:\home\agent`, run: `C:\run\rc`, executable: windowsExecutable, endpoint: `\\.\pipe\rc-kube`}
	case Darwin:
		run := path.Join(darwinHome, ".rc", "run")
		targetLayout = layout{home: darwinHome, run: run, executable: darwinExecutable, endpoint: path.Join(run, "rc-kube.sock")}
	default:
		return Runtime{}, fmt.Errorf("%w: %q", ErrUnsupportedOS, osName)
	}

	selector := maps.Clone(target.Placement.NodeSelector)
	if selector == nil {
		selector = make(map[string]string)
	}
	if selected := selector[corev1.LabelOSStable]; selected != "" && selected != string(osName) {
		return Runtime{}, fmt.Errorf("%w: runtime is %s but selector requires %s", ErrPlacementConflict, osName, selected)
	}
	selector[corev1.LabelOSStable] = string(osName)

	placement := Placement{
		NodeSelector:     selector,
		Tolerations:      slices.Clone(target.Placement.Tolerations),
		RuntimeClassName: cloneStringPointer(target.Placement.RuntimeClassName),
	}
	if target.Placement.Affinity != nil {
		placement.Affinity = target.Placement.Affinity.DeepCopy()
	}

	return Runtime{os: osName, layout: targetLayout, placement: placement}, nil
}

// FromPod restores the target runtime recorded on an existing Pod.
func FromPod(pod *corev1.Pod) (Runtime, error) {
	osName := corev1.OSName("")
	if pod.Spec.OS != nil {
		osName = pod.Spec.OS.Name
	} else {
		osName = corev1.OSName(pod.Spec.NodeSelector[corev1.LabelOSStable])
	}
	return Resolve(Target{OS: osName, Placement: Placement{
		NodeSelector: pod.Spec.NodeSelector, Tolerations: pod.Spec.Tolerations,
		Affinity: pod.Spec.Affinity, RuntimeClassName: pod.Spec.RuntimeClassName,
	}})
}

// OS returns the validated Kubernetes Pod OS.
func (runtime Runtime) OS() corev1.OSName { return runtime.os }

// ConfigMapMountPath returns the runtime-owned mount point for one ConfigMap.
func (runtime Runtime) ConfigMapMountPath(name string) string {
	return runtime.join(runtime.layout.run, "configmaps", name)
}

// SecretMountPath returns the runtime-owned mount point for one Secret.
func (runtime Runtime) SecretMountPath(name string) string {
	return runtime.join(runtime.layout.run, "secrets", name)
}

// ProcessTarget identifies the rc-kube bridge inside an existing Pod.
func (runtime Runtime) ProcessTarget(namespace, pod, container string) processruntime.Target {
	return processruntime.Target{
		Namespace: namespace, Pod: pod, Container: container,
		Executable: runtime.layout.executable, Endpoint: runtime.layout.endpoint,
	}
}

// DefaultDirectory selects the runtime-owned directory used when a process did
// not request an explicit working directory.
type DefaultDirectory uint8

const (
	WorkspaceDirectory DefaultDirectory = iota
	HomeDirectory
)

// AgentProfile selects the private home for one agent type and credential.
type AgentProfile struct {
	Type       string
	Credential string
}

// ProcessIntent contains portable process inputs. rc-kube derives native paths
// after the request reaches the target node.
type ProcessIntent struct {
	ID, UID            string
	Command            []string
	WorkingDirectory   string
	DefaultDirectory   DefaultDirectory
	TTY                bool
	Environment        map[string]string
	AgentProfile       *AgentProfile
	CredentialFiles    map[string][]byte
	CredentialMounts   []processruntime.CredentialMount
	ExposeCredentials  bool
	SSHConfigFragments map[string]string
}

// Process compiles one logical process into the rc-kube wire request.
func (runtime Runtime) Process(intent ProcessIntent) processruntime.StartRequest {
	environment := maps.Clone(intent.Environment)
	if environment == nil {
		environment = make(map[string]string)
	}
	var agentProfile *processruntime.AgentRef
	if intent.AgentProfile != nil && intent.AgentProfile.Type != "" {
		credential := intent.AgentProfile.Credential
		if credential == "" {
			credential = "default"
		}
		agentProfile = &processruntime.AgentRef{Type: intent.AgentProfile.Type, Credential: credential}
	}
	defaultDirectory := processruntime.DefaultDirectoryWorkspace
	if intent.DefaultDirectory == HomeDirectory {
		defaultDirectory = processruntime.DefaultDirectoryHome
	}

	return processruntime.StartRequest{
		ID: intent.ID, UID: intent.UID, Command: slices.Clone(intent.Command),
		WorkingDirectory: intent.WorkingDirectory, DefaultDirectory: defaultDirectory,
		TTY: intent.TTY, Environment: environment, Agent: agentProfile, ExposeCredentials: intent.ExposeCredentials,
		CredentialFiles: cloneFiles(intent.CredentialFiles), CredentialMounts: slices.Clone(intent.CredentialMounts),
		SSHConfigFragments: maps.Clone(intent.SSHConfigFragments),
	}
}

func (runtime Runtime) join(parts ...string) string {
	joined := path.Join(parts...)
	if runtime.os == corev1.Windows {
		return strings.ReplaceAll(joined, "/", `\`)
	}
	return joined
}

func cloneFiles(files map[string][]byte) map[string][]byte {
	if files == nil {
		return nil
	}
	result := make(map[string][]byte, len(files))
	for name, data := range files {
		result[name] = slices.Clone(data)
	}
	return result
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
