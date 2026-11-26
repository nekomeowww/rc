/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ActorPhase summarizes where an Actor is in its one-shot lifetime.
// +kubebuilder:validation:Enum=Pending;Starting;Running;Succeeded;Stopped;Failed
type ActorPhase string

const (
	ActorPhasePending   ActorPhase = "Pending"
	ActorPhaseStarting  ActorPhase = "Starting"
	ActorPhaseRunning   ActorPhase = "Running"
	ActorPhaseSucceeded ActorPhase = "Succeeded"
	ActorPhaseStopped   ActorPhase = "Stopped"
	ActorPhaseFailed    ActorPhase = "Failed"
)

// Actor condition types and reasons.
const (
	ActorConditionReady = ConditionReady

	ActorReasonWaitingForDaemon   = "WaitingForDaemon"
	ActorReasonNeedyNotConfigured = "NeedyNotConfigured"
	ActorReasonInvalidProfile     = "InvalidProfile"
	ActorReasonExecutorNotReady   = "ExecutorNotReady"
	ActorReasonWaitingForRunner   = "WaitingForRunner"
	ActorReasonRunnerFailed       = "RunnerFailed"
	ActorReasonCreatingRun        = "CreatingRun"
	ActorReasonRunning            = "Running"
	ActorReasonCompleted          = "Completed"
	ActorReasonRunFailed          = "RunFailed"
	ActorReasonRunLost            = "RunLost"
)

// Reasons recorded in status.stopReason.
const (
	ActorStopReasonIdle           = "Idle"
	ActorStopReasonMaxRunDuration = "MaxRunDuration"
	ActorStopReasonDeleted        = "Deleted"
)

// ActorProfile selects the needy profile of the run. rc passes the profile to
// the namespace's needy daemon, which validates it and resolves `extends`. rc
// only sets the runner of the actor and of each executor.
// +kubebuilder:validation:XValidation:rule="(has(self.builtin) ? 1 : 0) + (has(self.configMapKeyRef) ? 1 : 0) + (has(self.inline) ? 1 : 0) == 1",message="exactly one of builtin, configMapKeyRef, and inline must be set"
type ActorProfile struct {
	// builtin names a profile that ships with needy, such as codex or opencode.
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Builtin string `json:"builtin,omitempty"`

	// configMapKeyRef reads the profile YAML from a ConfigMap key in the
	// Actor's namespace.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`

	// inline is the profile YAML.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	Inline string `json:"inline,omitempty"`
}

// ActorExecutor binds one executor of the profile to a Workspace that serves
// as a needy executor (spec.needy.executor).
type ActorExecutor struct {
	// name is the executor's name in the profile's executors map.
	// +kubebuilder:validation:Pattern="^[A-Za-z0-9_-]+$"
	// +kubebuilder:validation:MaxLength=63
	// +required
	Name string `json:"name"`

	// workspaceRef names a Workspace in the Actor's namespace.
	// +required
	WorkspaceRef corev1.LocalObjectReference `json:"workspaceRef"`
}

// ActorUpstream sends the actor's internet traffic through a SOCKS5 proxy.
// The actor runner dials it for every connection that the gateway allows;
// a proxy that fails refuses the connection instead of dialing directly.
// With socks5h the proxy resolves names, so it must itself refuse private,
// cluster, and metadata addresses.
type ActorUpstream struct {
	// url is socks5h://HOST:PORT or socks5://HOST:PORT.
	// +kubebuilder:validation:Pattern=`^socks5h?://[^/@\s]+:[0-9]{1,5}$`
	// +kubebuilder:validation:MaxLength=255
	// +required
	URL string `json:"url"`

	// credentialsSecretRef names a Secret key in the Actor's namespace that
	// holds one line, USERNAME:PASSWORD. The runner reads it for every
	// connection, so a rotated Secret applies once the kubelet syncs it.
	// +optional
	CredentialsSecretRef *corev1.SecretKeySelector `json:"credentialsSecretRef,omitempty"`

	// podSelector selects the proxy Pods in the Actor's namespace, which the
	// actor Pod may then reach on the URL's port. A proxy inside the cluster
	// needs it: the actor Pod may not reach cluster addresses otherwise.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

// ActorSpec defines one needy run whose actor runs in a Firecracker VM on a
// KVM node. The spec is immutable.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Actor spec is immutable"
type ActorSpec struct {
	// profile is the needy profile of the run.
	// +required
	Profile ActorProfile `json:"profile"`

	// executors binds the profile's executors to Workspaces. Every executor of
	// the resolved profile must be bound; rc never places one on the daemon.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Executors []ActorExecutor `json:"executors,omitempty"`

	// args are appended to the profile's driving program.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Args []string `json:"args,omitempty"`

	// terminal runs the driving program on a terminal, so a client can attach
	// to it with `needy run attach`. Leave it unset for headless agents.
	// +optional
	Terminal bool `json:"terminal,omitempty"`

	// upstream sends the actor's internet traffic through a proxy, so it
	// leaves with the proxy's address instead of the node's.
	// +optional
	Upstream *ActorUpstream `json:"upstream,omitempty"`

	// resources of the actor runner Pod. They must cover the VM, Firecracker,
	// and the runner. The manager adds the KVM device.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// idleTimeout stops the run after this long without activity reported by
	// needy (guest calls, gateway traffic, routing decisions, terminal and exec
	// I/O). Zero or omitted disables it.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="idleTimeout must not be negative"
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// maxRunDuration stops the run this long after it started, whatever its
	// activity. It also bounds the actor runner's credential.
	// +kubebuilder:default="2h"
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="maxRunDuration must be positive"
	// +optional
	MaxRunDuration *metav1.Duration `json:"maxRunDuration,omitempty"`

	// deleteAfterCompleted deletes the Actor this long after its run ended.
	// Omitted keeps it.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="deleteAfterCompleted must not be negative"
	// +optional
	DeleteAfterCompleted *metav1.Duration `json:"deleteAfterCompleted,omitempty"`
}

// ActorStatus is read from the needy daemon's run record, never from the
// actor Pod's exit code.
type ActorStatus struct {
	// phase summarizes the Actor's lifetime.
	// +optional
	Phase ActorPhase `json:"phase,omitempty"`

	// runID is the needy run ID.
	// +optional
	RunID string `json:"runID,omitempty"`

	// runState is the needy run state, such as RUNNING or FINISHED.
	// +optional
	RunState string `json:"runState,omitempty"`

	// actorState is the needy actor state, such as READY or EXITED.
	// +optional
	ActorState string `json:"actorState,omitempty"`

	// exitCode is the driving program's exit code once it exited.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// runnerName is the needy runner that hosts the actor.
	// +optional
	RunnerName string `json:"runnerName,omitempty"`

	// podName is the actor runner Pod.
	// +optional
	PodName string `json:"podName,omitempty"`

	// startedAt is when the run was created.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// lastActivityTime is the run's last activity reported by needy.
	// +optional
	LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`

	// completedAt is when the run reached a terminal state.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// stopReason says why rc stopped the run: Idle, MaxRunDuration, or Deleted.
	// +optional
	StopReason string `json:"stopReason,omitempty"`

	// message is the needy failure or the reason the Actor cannot start.
	// +optional
	Message string `json:"message,omitempty"`

	// observedGeneration is the generation that the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe the Actor. Ready is True while the run is live.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsTerminal reports whether the phase is final.
func (phase ActorPhase) IsTerminal() bool {
	return phase == ActorPhaseSucceeded || phase == ActorPhaseStopped || phase == ActorPhaseFailed
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=".status.runID"
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=".status.podName"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Actor is one needy run whose actor runs in a Firecracker VM in its own Pod.
// Its commands go to Workspaces that serve as needy executors.
type Actor struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Actor
	// +required
	Spec ActorSpec `json:"spec"`

	// status defines the observed state of Actor
	// +optional
	Status ActorStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ActorList contains a list of Actor.
type ActorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Actor `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Actor{}, &ActorList{})
		return nil
	})
}
