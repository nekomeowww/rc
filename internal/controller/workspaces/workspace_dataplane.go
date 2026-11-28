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

package workspaces

import (
	"context"
	"fmt"

	"github.com/nekomeowww/xo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/apis/kubernetes/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/datad/gateway"
	"github.com/nekomeowww/rc/internal/datad/pki"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

const (
	dataPlaneCASecretName       = "rc-data-plane-ca"
	minDataPlaneAccessTokenSize = 32
)

func dataPlaneTokenSecretName(workspaceName string) string {
	return workspaceName + "-data-plane-token"
}

func dataPlaneServerCertSecretName(workspaceName string) string {
	return workspaceName + "-data-plane-tls"
}

// reconcileDataPlane prepares the access token and mTLS server certificate
// Secrets for a Workspace with spec.dataPlane.enabled == true.
func (r *WorkspaceReconciler) reconcileDataPlane(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (
	*rcplatform.WorkspaceDataPlanePodConfig,
	*workspacesv1alpha1.WorkspaceDataPlaneStatus,
	string,
	string,
	error,
) {
	if workspace.Spec.DataPlane == nil || !workspace.Spec.DataPlane.Enabled {
		return nil, nil, "", "", nil
	}

	tokenSecretName, reason, message, err := r.ensureDataPlaneAccessToken(ctx, workspace)
	if err != nil || reason != "" {
		return nil, nil, reason, message, err
	}

	certSecretName, err := r.ensureDataPlaneServerCert(ctx, workspace)
	if err != nil {
		return nil, nil, "", "", err
	}

	podConfig := &rcplatform.WorkspaceDataPlanePodConfig{
		CertSecretName: certSecretName,
		EnableSSH:      workspace.Spec.DataPlane.SSH != nil && workspace.Spec.DataPlane.SSH.Enabled,
	}
	status := &workspacesv1alpha1.WorkspaceDataPlaneStatus{
		AccessTokenSecretName: tokenSecretName,
		ServerCertSecretName:  certSecretName,
	}
	return podConfig, status, "", "", nil
}

func (r *WorkspaceReconciler) ensureDataPlaneAccessToken(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (string, string, string, error) {
	if ref := workspace.Spec.DataPlane.AccessTokenSecretRef; ref != nil {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: ref.Name}, secret)
		if apierrors.IsNotFound(err) {
			return "", workspacesv1alpha1.WorkspaceReasonAccessTokenNotFound,
				fmt.Sprintf("Data Plane access token Secret %s not found", ref.Name), nil
		}
		if err != nil {
			return "", "", "", err
		}
		key := ref.Key
		if key == "" {
			key = gateway.DefaultAccessTokenSecretKey
		}
		token, exists := secret.Data[key]
		if !exists {
			return "", workspacesv1alpha1.WorkspaceReasonAccessTokenNotFound,
				fmt.Sprintf("Data Plane access token Secret %s is missing key %q", ref.Name, key), nil
		}
		if len(token) < minDataPlaneAccessTokenSize {
			return "", workspacesv1alpha1.WorkspaceReasonInvalidAccessToken,
				fmt.Sprintf("Data Plane access token in Secret %s/%s must be at least %d bytes", ref.Name, key, minDataPlaneAccessTokenSize), nil
		}
		return ref.Name, "", "", nil
	}

	secretName := dataPlaneTokenSecretName(workspace.Name)
	existing := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: secretName}, existing)
	if getErr == nil {
		if len(existing.Data[gateway.DefaultAccessTokenSecretKey]) >= minDataPlaneAccessTokenSize {
			return secretName, "", "", nil
		}
	} else if !apierrors.IsNotFound(getErr) {
		return "", "", "", getErr
	}

	token, err := xo.RandomBase64Token(32)
	if err != nil {
		return "", "", "", fmt.Errorf("generate Data Plane access token: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: workspace.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			gateway.DefaultAccessTokenSecretKey: []byte(token),
		},
	}
	if err := controllerutil.SetControllerReference(workspace, secret, r.Scheme); err != nil {
		return "", "", "", err
	}
	if apierrors.IsNotFound(getErr) {
		if createErr := r.Create(ctx, secret); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return "", "", "", createErr
		}
	} else {
		existing.Data = secret.Data
		if updateErr := r.Update(ctx, existing); updateErr != nil {
			return "", "", "", updateErr
		}
	}
	return secretName, "", "", nil
}

func (r *WorkspaceReconciler) ensureDataPlaneCA(ctx context.Context, fallbackNamespace string) ([]byte, []byte, error) {
	caNamespace := r.DataPlaneCANamespace
	if caNamespace == "" {
		caNamespace = fallbackNamespace
	}
	caSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: caNamespace, Name: dataPlaneCASecretName}, caSecret)
	if err == nil && len(caSecret.Data["tls.crt"]) > 0 && len(caSecret.Data["tls.key"]) > 0 {
		return caSecret.Data["tls.crt"], caSecret.Data["tls.key"], nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}

	bundle, err := pki.GenerateCA(pki.DefaultCAValidity)
	if err != nil {
		return nil, nil, err
	}
	created := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataPlaneCASecretName,
			Namespace: caNamespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  bundle.CAPEM,
			"tls.crt": bundle.CertPEM,
			"tls.key": bundle.KeyPEM,
		},
	}
	if createErr := r.Create(ctx, created); createErr != nil {
		if apierrors.IsAlreadyExists(createErr) {
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: caNamespace, Name: dataPlaneCASecretName}, caSecret); getErr != nil {
				return nil, nil, getErr
			}
			return caSecret.Data["tls.crt"], caSecret.Data["tls.key"], nil
		}
		return nil, nil, createErr
	}
	return bundle.CertPEM, bundle.KeyPEM, nil
}

func (r *WorkspaceReconciler) ensureDataPlaneServerCert(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (string, error) {
	secretName := dataPlaneServerCertSecretName(workspace.Name)
	existing := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: secretName}, existing)
	if getErr == nil && len(existing.Data["ca.crt"]) > 0 && len(existing.Data["tls.crt"]) > 0 && len(existing.Data["tls.key"]) > 0 {
		return secretName, nil
	}
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return "", getErr
	}

	caCert, caKey, err := r.ensureDataPlaneCA(ctx, workspace.Namespace)
	if err != nil {
		return "", err
	}
	serverName := pki.WorkspaceServerName(workspace.Namespace, workspace.Name)
	bundle, err := pki.IssueServerCert(caCert, caKey, serverName, pki.DefaultLeafValidity)
	if err != nil {
		return "", err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: workspace.Namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"ca.crt":  bundle.CAPEM,
			"tls.crt": bundle.CertPEM,
			"tls.key": bundle.KeyPEM,
		},
	}
	if err := controllerutil.SetControllerReference(workspace, secret, r.Scheme); err != nil {
		return "", err
	}
	if apierrors.IsNotFound(getErr) {
		if createErr := r.Create(ctx, secret); createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return "", createErr
		}
	} else {
		existing.Data = secret.Data
		if updateErr := r.Update(ctx, existing); updateErr != nil {
			return "", updateErr
		}
	}
	return secretName, nil
}
