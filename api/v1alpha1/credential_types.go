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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CredentialType describes how a consumer presents credential data to a
// remote system.
// +kubebuilder:validation:Enum=SSHPrivateKey;HTTPBasicAuth;HTTPBearerToken;HTTPHeaders;Process
type CredentialType string

const (
	// CredentialTypeSSHPrivateKey identifies an SSH private key.
	CredentialTypeSSHPrivateKey CredentialType = "SSHPrivateKey"
	// CredentialTypeHTTPBasicAuth identifies HTTP Basic authentication data.
	CredentialTypeHTTPBasicAuth CredentialType = "HTTPBasicAuth"
	// CredentialTypeHTTPBearerToken identifies an HTTP Bearer token.
	CredentialTypeHTTPBearerToken CredentialType = "HTTPBearerToken"
	// CredentialTypeHTTPHeaders identifies named HTTP header values.
	CredentialTypeHTTPHeaders CredentialType = "HTTPHeaders"
	// CredentialTypeProcess identifies credentials projected into a process.
	CredentialTypeProcess CredentialType = "Process"
)

// SecretKeyReference selects one data entry from a Secret in the same
// namespace as the referring resource.
type SecretKeyReference struct {
	// name is the name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// key is the key of the Secret data entry containing the credential.
	// +kubebuilder:validation:MinLength=1
	// +required
	Key string `json:"key"`
}

// SSHPrivateKeyCredential identifies the private key used for SSH
// authentication.
type SSHPrivateKeyCredential struct {
	// privateKeyRef selects the private key from a Secret in the same namespace.
	// +required
	PrivateKeyRef SecretKeyReference `json:"privateKeyRef"`

	// knownHostsRef selects trusted SSH host keys from a Secret in the same
	// namespace.
	// +required
	KnownHostsRef SecretKeyReference `json:"knownHostsRef"`

	// config is an OpenSSH client configuration fragment. rc replaces the
	// ${identityFile} and ${knownHostsFile} placeholders with process-scoped
	// credential paths before exposing the fragment through ~/.ssh/config.
	// The fragment must not contain secret values.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	Config string `json:"config,omitempty"`
}

// HTTPBasicAuthCredential identifies the username and secret password or token
// used for HTTP Basic authentication.
type HTTPBasicAuthCredential struct {
	// username is presented as the HTTP Basic authentication username.
	// +kubebuilder:validation:MinLength=1
	// +required
	Username string `json:"username"`

	// passwordRef selects the password or token from a Secret in the same
	// namespace.
	// +required
	PasswordRef SecretKeyReference `json:"passwordRef"`
}

// HTTPBearerTokenCredential identifies the token presented through the HTTP
// Authorization header using the Bearer scheme.
type HTTPBearerTokenCredential struct {
	// tokenRef selects the Bearer token from a Secret in the same namespace.
	// +required
	TokenRef SecretKeyReference `json:"tokenRef"`
}

// HTTPHeader identifies one HTTP header whose value comes from a Secret.
type HTTPHeader struct {
	// name is the HTTP header name.
	// +kubebuilder:validation:Pattern="^[A-Za-z][A-Za-z0-9-]*$"
	// +required
	Name string `json:"name"`

	// valueRef selects the header value from a Secret in the same namespace.
	// +required
	ValueRef SecretKeyReference `json:"valueRef"`
}

// HTTPHeadersCredential identifies named secret values presented as HTTP
// request headers.
type HTTPHeadersCredential struct {
	// headers are the HTTP headers presented to the remote system.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	// +required
	Headers []HTTPHeader `json:"headers"`
}

// CredentialEnv projects one non-secret literal environment variable when a
// Credential is selected for a process.
type CredentialEnv struct {
	// name is the environment variable name.
	// +kubebuilder:validation:Pattern="^[A-Za-z_][A-Za-z0-9_]*$"
	// +required
	Name string `json:"name"`

	// value is the literal environment variable value.
	// +required
	Value string `json:"value"`
}

// CredentialFile projects raw Secret data to one process-scoped path.
type CredentialFile struct {
	// dataRef selects the raw file bytes from a Secret in the same namespace.
	// +required
	DataRef SecretKeyReference `json:"dataRef"`

	// mountPath is the absolute file path exposed while the process is alive.
	// +kubebuilder:validation:Pattern="^/.*"
	// +required
	MountPath string `json:"mountPath"`
}

// ProcessCredential configures independent file and environment projections.
// +kubebuilder:validation:XValidation:rule="(has(self.files) && size(self.files) > 0) || (has(self.envs) && size(self.envs) > 0)",message="at least one file or env entry is required"
type ProcessCredential struct {
	// files project raw Secret data to process-scoped paths.
	// +listType=map
	// +listMapKey=mountPath
	// +optional
	Files []CredentialFile `json:"files,omitempty"`

	// envs project non-secret literal environment variables. Explicit process
	// environment variables take precedence.
	// +listType=map
	// +listMapKey=name
	// +optional
	Envs []CredentialEnv `json:"envs,omitempty"`
}

// CredentialSpec defines the desired state of Credential.
// +kubebuilder:validation:XValidation:rule="self.type == 'SSHPrivateKey' ? has(self.sshPrivateKey) : !has(self.sshPrivateKey)",message="sshPrivateKey must be set exactly when type is SSHPrivateKey"
// +kubebuilder:validation:XValidation:rule="self.type == 'HTTPBasicAuth' ? has(self.httpBasicAuth) : !has(self.httpBasicAuth)",message="httpBasicAuth must be set exactly when type is HTTPBasicAuth"
// +kubebuilder:validation:XValidation:rule="self.type == 'HTTPBearerToken' ? has(self.httpBearerToken) : !has(self.httpBearerToken)",message="httpBearerToken must be set exactly when type is HTTPBearerToken"
// +kubebuilder:validation:XValidation:rule="self.type == 'HTTPHeaders' ? has(self.httpHeaders) : !has(self.httpHeaders)",message="httpHeaders must be set exactly when type is HTTPHeaders"
// +kubebuilder:validation:XValidation:rule="self.type == 'Process' ? has(self.process) : !has(self.process)",message="process must be set exactly when type is Process"
type CredentialSpec struct {
	// type describes how consumers present the referenced secret data.
	// +required
	Type CredentialType `json:"type"`

	// sshPrivateKey configures SSH private key authentication.
	// +optional
	SSHPrivateKey *SSHPrivateKeyCredential `json:"sshPrivateKey,omitempty"`

	// httpBasicAuth configures HTTP Basic authentication.
	// +optional
	HTTPBasicAuth *HTTPBasicAuthCredential `json:"httpBasicAuth,omitempty"`

	// httpBearerToken configures HTTP Bearer token authentication.
	// +optional
	HTTPBearerToken *HTTPBearerTokenCredential `json:"httpBearerToken,omitempty"`

	// httpHeaders configures named secret HTTP request headers.
	// +optional
	HTTPHeaders *HTTPHeadersCredential `json:"httpHeaders,omitempty"`

	// process configures file and environment projections for explicitly selected processes.
	// +optional
	Process *ProcessCredential `json:"process,omitempty"`
}

// CredentialStatus defines the observed state of Credential.
type CredentialStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Credential resource.
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

// Credential is the Schema for the credentials API
type Credential struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Credential
	// +required
	Spec CredentialSpec `json:"spec"`

	// status defines the observed state of Credential
	// +optional
	Status CredentialStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CredentialList contains a list of Credential
type CredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Credential `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Credential{}, &CredentialList{})
		return nil
	})
}
