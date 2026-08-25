package credentials

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

const githubCredentialSecretKey = "password"

// ImportGitHubRequest contains the local gh token to persist for GitHub Git
// over HTTPS authentication.
type ImportGitHubRequest struct {
	Namespace string
	Hostname  string
	Name      string
	Token     string
}

// GitHubImportResult identifies the Kubernetes objects changed by a GitHub
// credential import.
type GitHubImportResult struct {
	CredentialName      string
	SecretName          string
	SecretKey           string
	CredentialOperation controllerutil.OperationResult
	SecretOperation     controllerutil.OperationResult
}

// ImportGitHub creates or updates an HTTPBasicAuth Credential backed by the
// token returned by the local GitHub CLI.
func (importer *Importer) ImportGitHub(ctx context.Context, request ImportGitHubRequest) (GitHubImportResult, error) {
	hostname := strings.TrimSpace(request.Hostname)
	if hostname == "" {
		return GitHubImportResult{}, fmt.Errorf("GitHub hostname must not be empty")
	}
	if strings.ContainsAny(hostname, "/?#") {
		return GitHubImportResult{}, fmt.Errorf("invalid GitHub hostname %q", hostname)
	}

	token := strings.TrimSpace(request.Token)
	if token == "" {
		return GitHubImportResult{}, fmt.Errorf("GitHub token must not be empty")
	}

	credentialName := request.Name
	if credentialName == "" {
		var err error
		credentialName, err = githubCredentialName(hostname)
		if err != nil {
			return GitHubImportResult{}, err
		}
	}
	if errors := validation.IsDNS1123Subdomain(credentialName); len(errors) > 0 {
		return GitHubImportResult{}, fmt.Errorf("invalid Credential name %q: %s", credentialName, errors[0])
	}

	secretName := credentialName + "-auth"
	if len(secretName) > 253 {
		return GitHubImportResult{}, fmt.Errorf("credential name %q is too long for its Secret", credentialName)
	}

	credential := &configsv1alpha1.Credential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialName,
			Namespace: request.Namespace,
		},
	}
	credentialOperation, err := controllerutil.CreateOrUpdate(ctx, importer.client, credential, func() error {
		credential.Spec = configsv1alpha1.CredentialSpec{
			Type: configsv1alpha1.CredentialTypeHTTPBasicAuth,
			HTTPBasicAuth: &configsv1alpha1.HTTPBasicAuthCredential{
				Username: "git",
				PasswordRef: configsv1alpha1.SecretKeyReference{
					Name: secretName,
					Key:  githubCredentialSecretKey,
				},
			},
		}
		return nil
	})
	if err != nil {
		return GitHubImportResult{}, fmt.Errorf("persist GitHub Credential: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: request.Namespace,
		},
	}
	secretOperation, err := controllerutil.CreateOrUpdate(ctx, importer.client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		secret.Labels[managedByLabelName] = managedByRCCTL
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[githubCredentialSecretKey] = []byte(token)

		return controllerutil.SetControllerReference(credential, secret, importer.scheme)
	})
	if err != nil {
		return GitHubImportResult{}, fmt.Errorf("persist GitHub Secret: %w", err)
	}

	return GitHubImportResult{
		CredentialName:      credentialName,
		SecretName:          secretName,
		SecretKey:           githubCredentialSecretKey,
		CredentialOperation: credentialOperation,
		SecretOperation:     secretOperation,
	}, nil
}

func githubCredentialName(hostname string) (string, error) {
	var builder strings.Builder
	previousDash := false
	for _, character := range strings.ToLower(hostname) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			builder.WriteRune(character)
			previousDash = false
			continue
		}
		if !previousDash {
			builder.WriteByte('-')
			previousDash = true
		}
	}

	name := strings.Trim(builder.String(), "-")
	if len(name) > 253 {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(hostname)))[:12]
		name = strings.Trim(name[:253-len(digest)-1], "-") + "-" + digest
	}
	if errors := validation.IsDNS1123Subdomain(name); len(errors) > 0 {
		return "", fmt.Errorf("derive Credential name from GitHub hostname %q: %s", hostname, errors[0])
	}

	return name, nil
}
