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

package credentials

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

const fileCredentialSecretKey = "data"

// ImportProcessRequest describes raw file bytes and independent process projections.
type ImportProcessRequest struct {
	Namespace string
	Name      string
	Data      []byte
	MountPath string
	Envs      []configsv1alpha1.CredentialEnv
}

// ProcessImportResult identifies the Kubernetes objects changed by a process credential import.
type ProcessImportResult struct {
	CredentialName      string
	SecretName          string
	SecretKey           string
	CredentialOperation controllerutil.OperationResult
	SecretOperation     controllerutil.OperationResult
}

// ImportProcess stores raw bytes without interpreting or transforming them.
func (importer *Importer) ImportProcess(ctx context.Context, request ImportProcessRequest) (ProcessImportResult, error) {
	if validationErrors := validation.IsDNS1123Subdomain(request.Name); len(validationErrors) > 0 {
		return ProcessImportResult{}, fmt.Errorf("invalid Credential name %q: %s", request.Name, validationErrors[0])
	}
	if len(request.Data) == 0 {
		return ProcessImportResult{}, errors.New("credential file is empty")
	}
	if len(request.Data) > corev1.MaxSecretSize {
		return ProcessImportResult{}, fmt.Errorf("credential file exceeds the Kubernetes Secret limit of %d bytes", corev1.MaxSecretSize)
	}
	if !filepath.IsAbs(request.MountPath) || filepath.Clean(request.MountPath) != request.MountPath || request.MountPath == string(filepath.Separator) {
		return ProcessImportResult{}, fmt.Errorf("credential mount path %q must be a clean absolute file path", request.MountPath)
	}
	secretName := request.Name + "-file"
	if len(secretName) > 253 {
		return ProcessImportResult{}, fmt.Errorf("credential name %q is too long for its Secret", request.Name)
	}

	credential := &configsv1alpha1.Credential{ObjectMeta: metav1.ObjectMeta{Name: request.Name, Namespace: request.Namespace}}
	credentialOperation, err := controllerutil.CreateOrUpdate(ctx, importer.client, credential, func() error {
		credential.Spec = configsv1alpha1.CredentialSpec{
			Type: configsv1alpha1.CredentialTypeProcess,
			Process: &configsv1alpha1.ProcessCredential{
				Files: []configsv1alpha1.CredentialFile{{
					DataRef: configsv1alpha1.SecretKeyReference{Name: secretName, Key: fileCredentialSecretKey}, MountPath: request.MountPath,
				}},
				Envs: append([]configsv1alpha1.CredentialEnv(nil), request.Envs...),
			},
		}
		return nil
	})
	if err != nil {
		return ProcessImportResult{}, fmt.Errorf("persist process Credential: %w", err)
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: request.Namespace}}
	secretOperation, err := controllerutil.CreateOrUpdate(ctx, importer.client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		secret.Labels[managedByLabelName] = managedByRCCTL
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[fileCredentialSecretKey] = append([]byte(nil), request.Data...)
		return controllerutil.SetControllerReference(credential, secret, importer.scheme)
	})
	if err != nil {
		return ProcessImportResult{}, fmt.Errorf("persist process Credential Secret: %w", err)
	}

	return ProcessImportResult{
		CredentialName: request.Name, SecretName: secretName, SecretKey: fileCredentialSecretKey,
		CredentialOperation: credentialOperation, SecretOperation: secretOperation,
	}, nil
}
