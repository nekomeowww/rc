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

package agentprocess

import (
	"context"
	"errors"
)

// ErrNotFound reports that the original supervisor process no longer exists.
var ErrNotFound = errors.New("process not found")

// Target identifies one rc-kube supervisor Pod.
type Target struct {
	Namespace string
	Pod       string
	Container string
}

// CredentialMount projects one temporary credential source to a process path.
type CredentialMount struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// StartRequest is the versioned process contract sent to rc-kube.
type StartRequest struct {
	ID               string            `json:"id"`
	UID              string            `json:"uid"`
	Command          []string          `json:"command"`
	WorkingDirectory string            `json:"workingDirectory"`
	TTY              bool              `json:"tty"`
	Environment      map[string]string `json:"environment,omitempty"`
	AgentHome        string            `json:"agentHome,omitempty"`
	CredentialFiles  map[string][]byte `json:"credentialFiles,omitempty"`
	CredentialMounts []CredentialMount `json:"credentialMounts,omitempty"`
	RuntimeDirectory string            `json:"runtimeDirectory,omitempty"`
	CredentialsRoot  string            `json:"credentialsRoot,omitempty"`
	TranscriptPath   string            `json:"transcriptPath"`
}

// State is the supervisor's observable process state.
type State struct {
	ID              string `json:"id"`
	UID             string `json:"uid"`
	Phase           string `json:"phase"`
	PID             int    `json:"pid,omitempty"`
	ExitCode        *int32 `json:"exitCode,omitempty"`
	Reason          string `json:"reason,omitempty"`
	AttachedClients int32  `json:"attachedClients,omitempty"`
}

// Runtime controls processes at the Pod exec system boundary.
type Runtime interface {
	Start(context.Context, Target, StartRequest) (State, error)
	Inspect(context.Context, Target, string) (State, error)
	Stop(context.Context, Target, string) (State, error)
}
