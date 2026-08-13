package credentials

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

const (
	codexCredentialName = "codex"
	codexSecretName     = "codex-auth"
	codexSecretKey      = "auth.json"
)

// ImportAgentRequest contains the identity and secret bytes needed to import
// one agent credential.
type ImportAgentRequest struct {
	Namespace string
	Agent     configsv1alpha1.AgentType
	Data      []byte
}

// ImportResult identifies the Kubernetes objects changed by an import.
type ImportResult struct {
	AgentCredentialName      string
	SecretName               string
	SecretKey                string
	AgentCredentialOperation controllerutil.OperationResult
	SecretOperation          controllerutil.OperationResult
}

// Importer persists credential resources through the Kubernetes API.
type Importer struct {
	client client.Client
	scheme *runtime.Scheme
}

// NewImporter creates an importer at the Kubernetes API boundary.
func NewImporter(kubeClient client.Client, scheme *runtime.Scheme) *Importer {
	return &Importer{
		client: kubeClient,
		scheme: scheme,
	}
}

// ImportAgent creates or updates the AgentCredential and its owned Secret.
func (importer *Importer) ImportAgent(ctx context.Context, request ImportAgentRequest) (ImportResult, error) {
	if request.Namespace == "" {
		return ImportResult{}, errors.New("namespace is required")
	}
	if request.Agent != configsv1alpha1.AgentTypeCodex {
		return ImportResult{}, fmt.Errorf("unsupported agent %q", request.Agent)
	}
	if len(request.Data) == 0 {
		return ImportResult{}, errors.New("credential file is empty")
	}
	if len(request.Data) > corev1.MaxSecretSize {
		return ImportResult{}, fmt.Errorf("credential file exceeds the Kubernetes Secret limit of %d bytes", corev1.MaxSecretSize)
	}

	agentCredential := &configsv1alpha1.AgentCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      codexCredentialName,
			Namespace: request.Namespace,
		},
	}
	agentCredentialOperation, err := controllerutil.CreateOrUpdate(ctx, importer.client, agentCredential, func() error {
		agentCredential.Spec.Agent = request.Agent
		agentCredential.Spec.SecretKeyRef = configsv1alpha1.SecretKeyReference{
			Name: codexSecretName,
			Key:  codexSecretKey,
		}
		return nil
	})
	if err != nil {
		return ImportResult{}, fmt.Errorf("persist AgentCredential: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      codexSecretName,
			Namespace: request.Namespace,
		},
	}
	secretOperation, err := controllerutil.CreateOrUpdate(ctx, importer.client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		secret.Labels["app.kubernetes.io/managed-by"] = "rcctl"
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[codexSecretKey] = append([]byte(nil), request.Data...)
		return controllerutil.SetControllerReference(agentCredential, secret, importer.scheme)
	})
	if err != nil {
		return ImportResult{}, fmt.Errorf("persist Secret: %w", err)
	}

	return ImportResult{
		AgentCredentialName:      codexCredentialName,
		SecretName:               codexSecretName,
		SecretKey:                codexSecretKey,
		AgentCredentialOperation: agentCredentialOperation,
		SecretOperation:          secretOperation,
	}, nil
}
