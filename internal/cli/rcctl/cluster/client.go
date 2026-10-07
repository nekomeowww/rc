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

package cluster

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

// Client bundles the Kubernetes clients rcctl commands use. Constructing it
// performs no network I/O.
type Client struct {
	Kube       client.Client
	Kubernetes kubernetes.Interface
	Processes  *processruntime.KubeRuntime
	Config     *rest.Config
}

func New(config *rest.Config) (*Client, error) {
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes clientset: %w", err)
	}
	podExecutor, err := processruntime.NewKubernetesPodExecutor(config)
	if err != nil {
		return nil, err
	}

	return &Client{Kube: kubeClient, Kubernetes: clientset, Processes: processruntime.NewKubeRuntime(podExecutor), Config: rest.CopyConfig(config)}, nil
}

// Connect resolves the kubeconfig flags and builds a Client for the selected
// cluster, returning the resolved namespace alongside it.
func Connect(flags *kubeconfig.Flags) (*Client, string, error) {
	config, namespace, err := flags.Resolve()
	if err != nil {
		return nil, "", err
	}
	clusterClient, err := New(config)
	if err != nil {
		return nil, "", err
	}

	return clusterClient, namespace, nil
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"batch": batchv1.AddToScheme, "coordination": coordinationv1.AddToScheme, "core": corev1.AddToScheme, "RBAC": rbacv1.AddToScheme,
		"configs": configsv1alpha1.AddToScheme, "repositories": repositoriesv1alpha1.AddToScheme,
		"workspaces": workspacesv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("register %s API types: %w", name, err)
		}
	}

	return scheme, nil
}
