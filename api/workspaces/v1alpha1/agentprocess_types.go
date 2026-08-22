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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentProcessTargetKind identifies the runtime owning a process.
// +kubebuilder:validation:Enum=Workspace;WorkspaceEnvironment
type AgentProcessTargetKind string

// AgentProcessDesiredState is one-way from Running to Stopped.
// +kubebuilder:validation:Enum=Running;Stopped
type AgentProcessDesiredState string

// AgentProcessPhase is the observed process lifecycle.
// +kubebuilder:validation:Enum=Pending;Starting;Running;Succeeded;Failed;Stopped;Lost
type AgentProcessPhase string

const (
	AgentProcessTargetWorkspace            AgentProcessTargetKind   = "Workspace"
	AgentProcessTargetWorkspaceEnvironment AgentProcessTargetKind   = "WorkspaceEnvironment"
	AgentProcessDesiredStateRunning        AgentProcessDesiredState = "Running"
	AgentProcessDesiredStateStopped        AgentProcessDesiredState = "Stopped"
	AgentProcessPhasePending               AgentProcessPhase        = "Pending"
	AgentProcessPhaseStarting              AgentProcessPhase        = "Starting"
	AgentProcessPhaseRunning               AgentProcessPhase        = "Running"
	AgentProcessPhaseSucceeded             AgentProcessPhase        = "Succeeded"
	AgentProcessPhaseFailed                AgentProcessPhase        = "Failed"
	AgentProcessPhaseStopped               AgentProcessPhase        = "Stopped"
	AgentProcessPhaseLost                  AgentProcessPhase        = "Lost"
	AgentProcessConditionReady                                      = ConditionReady
)

// AgentProcessTargetReference selects a Workspace or Environment draft.
type AgentProcessTargetReference struct {
	// kind is Workspace or WorkspaceEnvironment. Environment targets always
	// address the mutable draft.
	// +required
	Kind AgentProcessTargetKind `json:"kind"`

	// name is the target metadata.name in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// ProcessEnvironmentVariable references one value in the temporary env Secret.
type ProcessEnvironmentVariable struct {
	// name is the environment variable name.
	// +kubebuilder:validation:Pattern="^[A-Za-z_][A-Za-z0-9_]*$"
	// +required
	Name string `json:"name"`

	// key is the data key in envSecretRef. It defaults to name.
	// +optional
	Key string `json:"key,omitempty"`
}

// AgentProcessSpec defines one immutable, at-most-once command.
// +kubebuilder:validation:XValidation:rule="self.targetRef == oldSelf.targetRef && self.command == oldSelf.command && has(self.workingDirectory) == has(oldSelf.workingDirectory) && (!has(self.workingDirectory) || self.workingDirectory == oldSelf.workingDirectory) && has(self.tty) == has(oldSelf.tty) && (!has(self.tty) || self.tty == oldSelf.tty) && has(self.envSecretRef) == has(oldSelf.envSecretRef) && (!has(self.envSecretRef) || self.envSecretRef == oldSelf.envSecretRef) && has(self.env) == has(oldSelf.env) && (!has(self.env) || self.env == oldSelf.env) && has(self.agentType) == has(oldSelf.agentType) && (!has(self.agentType) || self.agentType == oldSelf.agentType) && has(self.agentCredentialRef) == has(oldSelf.agentCredentialRef) && (!has(self.agentCredentialRef) || self.agentCredentialRef == oldSelf.agentCredentialRef) && has(self.credentialRefs) == has(oldSelf.credentialRefs) && (!has(self.credentialRefs) || self.credentialRefs == oldSelf.credentialRefs)",message="process execution fields are immutable"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.desiredState) && oldSelf.desiredState == 'Stopped' ? has(self.desiredState) && self.desiredState == 'Stopped' : true",message="a stopped process cannot return to Running"
type AgentProcessSpec struct {
	// targetRef selects the owning runtime.
	// +required
	TargetRef AgentProcessTargetReference `json:"targetRef"`

	// command is the exact argv executed by rc-kube.
	// +kubebuilder:validation:MinItems=1
	// +required
	Command []string `json:"command"`

	// workingDirectory overrides the target default.
	// +optional
	WorkingDirectory string `json:"workingDirectory,omitempty"`

	// tty allocates a PTY and enables interactive attach.
	// +optional
	TTY bool `json:"tty,omitempty"`

	// desiredState requests process start or one-way termination.
	// +kubebuilder:default=Running
	// +optional
	DesiredState AgentProcessDesiredState `json:"desiredState,omitempty"`

	// envSecretRef selects the temporary Secret holding caller environment.
	// +optional
	EnvSecretRef *LocalReference `json:"envSecretRef,omitempty"`

	// env maps variables to keys in envSecretRef.
	// +listType=map
	// +listMapKey=name
	// +optional
	Env []ProcessEnvironmentVariable `json:"env,omitempty"`

	// agentType selects a recognized adapter such as codex.
	// +optional
	AgentType string `json:"agentType,omitempty"`

	// agentCredentialRef selects one Workspace AgentCredential.
	// +optional
	AgentCredentialRef *LocalReference `json:"agentCredentialRef,omitempty"`

	// credentialRefs explicitly exposes named generic Credentials.
	// +optional
	CredentialRefs []LocalReference `json:"credentialRefs,omitempty"`
}

// AgentProcessStatus defines the observed state of AgentProcess.
type AgentProcessStatus struct {
	// observedGeneration is the latest generation reflected by status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is the process lifecycle state.
	// +optional
	Phase AgentProcessPhase `json:"phase,omitempty"`

	// runtimePodName is the Pod that owns the original process.
	// +optional
	RuntimePodName string `json:"runtimePodName,omitempty"`

	// runtimePodUID prevents a replacement Pod from being mistaken for the
	// original process owner.
	// +optional
	RuntimePodUID string `json:"runtimePodUID,omitempty"`

	// startedAt is when rc-kube acknowledged ownership.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is when the process reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// exitCode is the child process exit code when one was observed.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// terminationReason describes why the process ended.
	// +optional
	TerminationReason string `json:"terminationReason,omitempty"`

	// attachedClients is the latest supervisor client count.
	// +optional
	AttachedClients int32 `json:"attachedClients,omitempty"`

	// transcriptPath is relative to the target persistent home.
	// +optional
	TranscriptPath string `json:"transcriptPath,omitempty"`

	// conditions represent the current state of the AgentProcess resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:selectablefield:JSONPath=".spec.targetRef.name"
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.targetRef.name"
// +kubebuilder:printcolumn:name="TTY",type=boolean,JSONPath=".spec.tty"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Exit",type=integer,JSONPath=".status.exitCode"
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=".status.startedAt"

// AgentProcess is the Schema for the agentprocesses API
type AgentProcess struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AgentProcess
	// +required
	Spec AgentProcessSpec `json:"spec"`

	// status defines the observed state of AgentProcess
	// +optional
	Status AgentProcessStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AgentProcessList contains a list of AgentProcess
type AgentProcessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AgentProcess `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AgentProcess{}, &AgentProcessList{})
		return nil
	})
}
