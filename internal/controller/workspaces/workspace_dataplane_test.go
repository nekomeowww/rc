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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/datad/gateway"
)

func TestReconcileDataPlaneCreatesAndReusesSecrets(t *testing.T) {
	t.Parallel()

	const (
		wsNamespace = "dataplane-test"
		caNamespace = "dataplane-ca"
	)

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))

	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: wsNamespace,
			UID:       "ws-uid-1",
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DataPlane: &workspacesv1alpha1.WorkspaceDataPlaneSpec{
				Enabled: true,
			},
		},
	}

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace).
		Build()

	reconciler := &WorkspaceReconciler{
		Client:               kubeClient,
		Scheme:               scheme,
		DataPlaneCANamespace: caNamespace,
	}

	ctx := context.Background()
	podCfg, status, reason, message, err := reconciler.reconcileDataPlane(ctx, workspace)
	require.NoError(t, err)
	require.Empty(t, reason)
	require.Empty(t, message)
	require.NotNil(t, podCfg)
	require.NotNil(t, status)
	require.Equal(t, "demo-data-plane-token", status.AccessTokenSecretName)
	require.Equal(t, "demo-data-plane-tls", status.ServerCertSecretName)
	require.Equal(t, "demo-data-plane-tls", podCfg.CertSecretName)

	caSecret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: caNamespace, Name: dataPlaneCASecretName}, caSecret))
	require.NotEmpty(t, caSecret.Data["ca.crt"])
	require.NotEmpty(t, caSecret.Data["tls.crt"])
	require.NotEmpty(t, caSecret.Data["tls.key"])

	tokenSecret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "demo-data-plane-token"}, tokenSecret))
	firstToken := string(tokenSecret.Data[gateway.DefaultAccessTokenSecretKey])
	require.GreaterOrEqual(t, len(firstToken), minDataPlaneAccessTokenSize)

	tlsSecret := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "demo-data-plane-tls"}, tlsSecret))
	firstCert := string(tlsSecret.Data["tls.crt"])
	require.NotEmpty(t, tlsSecret.Data["ca.crt"])
	require.NotEmpty(t, firstCert)
	require.NotEmpty(t, tlsSecret.Data["tls.key"])

	// Re-running reconcileDataPlane must be idempotent and keep the same token and cert.
	_, _, reason, _, err = reconciler.reconcileDataPlane(ctx, workspace)
	require.NoError(t, err)
	require.Empty(t, reason)

	tokenSecret2 := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "demo-data-plane-token"}, tokenSecret2))
	require.Equal(t, firstToken, string(tokenSecret2.Data[gateway.DefaultAccessTokenSecretKey]))

	tlsSecret2 := &corev1.Secret{}
	require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: wsNamespace, Name: "demo-data-plane-tls"}, tlsSecret2))
	require.Equal(t, firstCert, string(tlsSecret2.Data["tls.crt"]))
}
