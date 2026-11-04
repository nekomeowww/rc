/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package cluster

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestRESTMapperMatchesEveryRCCustomResourceDefinition(t *testing.T) {
	t.Parallel()
	scheme, err := newScheme()
	require.NoError(t, err, "build the rcctl Kubernetes scheme")
	mapper := newRESTMapper(scheme, meta.NewDefaultRESTMapper(nil))
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "config", "crd", "bases", "*.yaml"))
	require.NoError(t, err, "list CRD manifests")
	require.NotEmpty(t, files, "the CRD manifests exist")

	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err, "read %s", file)
		definition := new(apiextensionsv1.CustomResourceDefinition)
		require.NoError(t, yaml.Unmarshal(data, definition), "decode %s", file)
		wantScope := meta.RESTScopeNameNamespace
		if definition.Spec.Scope == apiextensionsv1.ClusterScoped {
			wantScope = meta.RESTScopeNameRoot
		}
		for _, version := range definition.Spec.Versions {
			kind := schema.GroupKind{Group: definition.Spec.Group, Kind: definition.Spec.Names.Kind}
			mapping, err := mapper.RESTMapping(kind, version.Name)
			require.NoError(t, err, "map %s/%s without discovery", kind, version.Name)
			assert.Equal(t, definition.Spec.Names.Plural, mapping.Resource.Resource, "plural of %s", kind)
			assert.Equal(t, wantScope, mapping.Scope.Name(), "scope of %s", kind)
		}
	}
}

func TestRESTMapperMapsBuiltInKinds(t *testing.T) {
	t.Parallel()
	scheme, err := newScheme()
	require.NoError(t, err, "build the rcctl Kubernetes scheme")
	mapper := newRESTMapper(scheme, meta.NewDefaultRESTMapper(nil))

	for _, test := range []struct {
		name     string
		kind     schema.GroupVersionKind
		resource string
		scope    meta.RESTScopeName
	}{
		{name: "NamespacedPod", kind: corev1.SchemeGroupVersion.WithKind("Pod"), resource: "pods", scope: meta.RESTScopeNameNamespace},
		{name: "ClusterScopedNamespace", kind: corev1.SchemeGroupVersion.WithKind("Namespace"), resource: "namespaces", scope: meta.RESTScopeNameRoot},
		{name: "IrregularEndpoints", kind: corev1.SchemeGroupVersion.WithKind("Endpoints"), resource: "endpoints", scope: meta.RESTScopeNameNamespace},
		{name: "ClusterScopedClusterRole", kind: schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, resource: "clusterroles", scope: meta.RESTScopeNameRoot},
		{name: "NamespacedLease", kind: schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"}, resource: "leases", scope: meta.RESTScopeNameNamespace},
		{name: "NamespacedJob", kind: schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, resource: "jobs", scope: meta.RESTScopeNameNamespace},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mapping, err := mapper.RESTMapping(test.kind.GroupKind(), test.kind.Version)
			require.NoError(t, err, "map without discovery")
			assert.Equal(t, test.resource, mapping.Resource.Resource, "resource")
			assert.Equal(t, test.scope, mapping.Scope.Name(), "scope")
		})
	}
}

func TestRESTMapperFallsBackForKindsOutsideTheScheme(t *testing.T) {
	t.Parallel()
	scheme, err := newScheme()
	require.NoError(t, err, "build the rcctl Kubernetes scheme")
	deployment := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	fallback := meta.NewDefaultRESTMapper(nil)
	fallback.Add(deployment, meta.RESTScopeNamespace)

	mapping, err := newRESTMapper(scheme, fallback).RESTMapping(deployment.GroupKind(), deployment.Version)
	require.NoError(t, err, "the fallback maps a kind that rcctl does not register")
	assert.Equal(t, "deployments", mapping.Resource.Resource)
}

func TestClientSendsRequestsWithoutDiscoveryOverOneConnection(t *testing.T) {
	t.Parallel()
	var connections, discoveryRequests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api" || request.URL.Path == "/apis" {
			discoveryRequests.Add(1)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"runtime","namespace":"rc-dev"}}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	// A proxy setting disables the transport cache of client-go, which is the
	// case where separate clients open separate connections.
	clusterClient, err := New(&rest.Config{
		Host:  server.URL,
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	})
	require.NoError(t, err, "build the rcctl clients")

	require.NoError(t, clusterClient.Kube.Get(t.Context(), client.ObjectKey{Namespace: "rc-dev", Name: "runtime"}, new(corev1.Pod)), "get a Pod through the controller-runtime client")
	_, err = clusterClient.Kubernetes.CoreV1().Pods("rc-dev").Get(t.Context(), "runtime", metav1.GetOptions{})
	require.NoError(t, err, "get a Pod through the clientset")
	_, err = clusterClient.Kube.RESTMapper().RESTMapping(workspacesv1alpha1.GroupVersion.WithKind("Workspace").GroupKind())
	require.NoError(t, err, "map a Workspace")

	assert.Equal(t, int32(0), discoveryRequests.Load(), "known kinds need no discovery")
	assert.Equal(t, int32(1), connections.Load(), "the clients share one connection")
}
