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

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

type Client struct {
	Kube      client.Client
	Processes *processruntime.KubeRuntime
	Config    *rest.Config
}

func New(config *rest.Config) (*Client, error) {
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"core": corev1.AddToScheme, "RBAC": rbacv1.AddToScheme,
		"configs": configsv1alpha1.AddToScheme, "repositories": repositoriesv1alpha1.AddToScheme,
		"workspaces": workspacesv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("register %s API types: %w", name, err)
		}
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	podExecutor, err := processruntime.NewKubernetesPodExecutor(config)
	if err != nil {
		return nil, err
	}

	return &Client{Kube: kubeClient, Processes: processruntime.NewKubeRuntime(podExecutor), Config: rest.CopyConfig(config)}, nil
}
