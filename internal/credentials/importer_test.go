package credentials

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

const githubTestSecretName = "github-com-auth"

func TestImportAgentCreatesAndUpdatesCredentialObjects(t *testing.T) {
	t.Parallel()
	const namespace = "agents"

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core Kubernetes types")
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register rc API types")

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	importer := NewImporter(kubeClient, scheme)
	request := ImportAgentRequest{
		Namespace: namespace,
		Agent:     configsv1alpha1.AgentTypeCodex,
		Data:      []byte(`{"auth":"first"}`),
	}

	result, err := importer.ImportAgent(context.Background(), request)
	require.NoError(t, err, "import Codex credential")
	assert.Equal(t, codexCredentialName, result.AgentCredentialName)
	assert.Equal(t, codexSecretName, result.SecretName)
	assert.Equal(t, codexSecretKey, result.SecretKey)

	agentCredential := &configsv1alpha1.AgentCredential{}
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      codexCredentialName,
	}, agentCredential), "get imported AgentCredential")
	assert.Equal(t, configsv1alpha1.AgentTypeCodex, agentCredential.Spec.Agent)
	assert.Equal(t, configsv1alpha1.SecretKeyReference{
		Name: codexSecretName,
		Key:  codexSecretKey,
	}, agentCredential.Spec.SecretKeyRef)

	secret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      codexSecretName,
	}, secret), "get imported Secret")
	assert.Equal(t, []byte(`{"auth":"first"}`), secret.Data[codexSecretKey])
	require.Len(t, secret.OwnerReferences, 1, "Secret is owned by AgentCredential")
	assert.Equal(t, codexCredentialName, secret.OwnerReferences[0].Name)

	secret.Data["preserved"] = []byte("keep")
	require.NoError(t, kubeClient.Update(context.Background(), secret), "add unrelated Secret data")
	request.Data = []byte(`{"auth":"second"}`)
	_, err = importer.ImportAgent(context.Background(), request)
	require.NoError(t, err, "repeat import updates existing objects")

	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      codexSecretName,
	}, secret), "get updated Secret")
	assert.Equal(t, []byte(`{"auth":"second"}`), secret.Data[codexSecretKey])
	assert.Equal(t, []byte("keep"), secret.Data["preserved"], "preserve unrelated Secret data")
}

func TestImportProcessPreservesRawBytesAndIndependentProjections(t *testing.T) {
	t.Parallel()
	const namespace = "credentials"
	const credentialName = "tool-auth"
	raw := []byte{0x00, 0x01, '{', '\n', 0xff}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core Kubernetes types")
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register rc API types")

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	importer := NewImporter(kubeClient, scheme)
	result, err := importer.ImportProcess(context.Background(), ImportProcessRequest{
		Namespace: namespace, Name: credentialName, Data: raw, MountPath: "/home/agent/.tool/credentials.json",
		Envs: []configsv1alpha1.CredentialEnv{{Name: "TOOL_HOME", Value: "/home/agent/.tool"}},
	})
	require.NoError(t, err, "import raw file credential")
	assert.Equal(t, credentialName, result.CredentialName)
	assert.Equal(t, credentialName+"-file", result.SecretName)
	assert.Equal(t, fileCredentialSecretKey, result.SecretKey)

	credential := new(configsv1alpha1.Credential)
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      credentialName,
	}, credential), "get imported file Credential")
	require.NotNil(t, credential.Spec.Process, "process Credential has projection configuration")
	assert.Equal(t, []configsv1alpha1.CredentialFile{{
		DataRef:   configsv1alpha1.SecretKeyReference{Name: credentialName + "-file", Key: fileCredentialSecretKey},
		MountPath: "/home/agent/.tool/credentials.json",
	}}, credential.Spec.Process.Files)
	assert.Equal(t, []configsv1alpha1.CredentialEnv{{Name: "TOOL_HOME", Value: "/home/agent/.tool"}}, credential.Spec.Process.Envs)

	secret := new(corev1.Secret)
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      credentialName + "-file",
	}, secret), "get imported file Secret")
	assert.Equal(t, raw, secret.Data[fileCredentialSecretKey], "store input bytes without transformation")
}

func TestImportAgentRejectsUnsupportedAgent(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core Kubernetes types")
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register rc API types")

	importer := NewImporter(fake.NewClientBuilder().WithScheme(scheme).Build(), scheme)
	result, err := importer.ImportAgent(context.Background(), ImportAgentRequest{
		Namespace: "agents",
		Agent:     "other",
		Data:      []byte("credential"),
	})

	require.Error(t, err)
	assert.Empty(t, result)
}

func TestImportGitHubCreatesAndUpdatesCredentialObjects(t *testing.T) {
	t.Parallel()
	const namespace = "repositories"

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core Kubernetes types")
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register rc API types")

	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	importer := NewImporter(kubeClient, scheme)
	request := ImportGitHubRequest{
		Namespace: namespace,
		Hostname:  "github.com",
		Token:     "ghp_first",
	}

	result, err := importer.ImportGitHub(context.Background(), request)
	require.NoError(t, err, "import GitHub credential")
	assert.Equal(t, "github-com", result.CredentialName)
	assert.Equal(t, "github-com-auth", result.SecretName)
	assert.Equal(t, "password", result.SecretKey)

	credential := &configsv1alpha1.Credential{}
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      "github-com",
	}, credential), "get imported Credential")
	assert.Equal(t, configsv1alpha1.CredentialTypeHTTPBasicAuth, credential.Spec.Type)
	assert.Equal(t, "git", credential.Spec.HTTPBasicAuth.Username)
	assert.Equal(t, configsv1alpha1.SecretKeyReference{
		Name: githubTestSecretName,
		Key:  "password",
	}, credential.Spec.HTTPBasicAuth.PasswordRef)

	secret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      githubTestSecretName,
	}, secret), "get imported Secret")
	assert.Equal(t, []byte("ghp_first"), secret.Data["password"])
	require.Len(t, secret.OwnerReferences, 1, "Secret is owned by Credential")
	assert.Equal(t, "github-com", secret.OwnerReferences[0].Name)

	secret.Data["preserved"] = []byte("keep")
	require.NoError(t, kubeClient.Update(context.Background(), secret), "add unrelated Secret data")
	request.Token = "ghp_second"
	_, err = importer.ImportGitHub(context.Background(), request)
	require.NoError(t, err, "repeat import updates existing objects")

	require.NoError(t, kubeClient.Get(context.Background(), types.NamespacedName{
		Namespace: namespace,
		Name:      githubTestSecretName,
	}, secret), "get updated Secret")
	assert.Equal(t, []byte("ghp_second"), secret.Data["password"])
	assert.Equal(t, []byte("keep"), secret.Data["preserved"], "preserve unrelated Secret data")
}
