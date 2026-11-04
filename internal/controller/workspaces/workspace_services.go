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
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const reasonInvalidServiceEnvironment = "InvalidServiceEnvironment"

// compileWorkspaceServices turns spec.services into the rc-kube wire form.
// Each service receives the Workspace env, which a WorkspaceExec also receives
// by default, overridden by its own env. A value read from a Secret or
// ConfigMap stays a reference: it becomes a runtime container variable that
// Kubernetes resolves at container start, so no secret value enters the Pod
// spec. The result is deterministic, because it is part of the runtime
// topology hash.
func compileWorkspaceServices(workspace *workspacesv1alpha1.Workspace) ([]processruntime.Service, []corev1.EnvVar, string, string) {
	if len(workspace.Spec.Services) == 0 {
		return nil, nil, "", ""
	}
	services := make([]processruntime.Service, 0, len(workspace.Spec.Services))
	var containerEnvironment []corev1.EnvVar
	for _, service := range workspace.Spec.Services {
		variables := make(map[string]corev1.EnvVar, len(workspace.Spec.Env)+len(service.Env))
		for _, variable := range workspace.Spec.Env {
			variables[variable.Name] = variable
		}
		for _, variable := range service.Env {
			variables[variable.Name] = variable
		}
		names := make([]string, 0, len(variables))
		for name := range variables {
			names = append(names, name)
		}
		slices.Sort(names)
		compiled := processruntime.Service{
			Name:             service.Name,
			Command:          slices.Clone(service.Command),
			WorkingDirectory: service.WorkingDirectory,
		}
		if compiled.WorkingDirectory == "" {
			compiled.WorkingDirectory = workspace.Spec.DefaultWorkingDirectory
		}
		for _, name := range names {
			variable := variables[name]
			if variable.ValueFrom == nil {
				if compiled.Environment == nil {
					compiled.Environment = make(map[string]string)
				}
				compiled.Environment[name] = variable.Value
				continue
			}
			if variable.ValueFrom.SecretKeyRef == nil && variable.ValueFrom.ConfigMapKeyRef == nil {
				return nil, nil, reasonInvalidServiceEnvironment, fmt.Sprintf("Service %s variable %s must come from a value, a Secret key, or a ConfigMap key", service.Name, name)
			}
			containerName := processruntime.ServiceEnvironmentPrefix + strconv.Itoa(len(containerEnvironment))
			containerEnvironment = append(containerEnvironment, corev1.EnvVar{
				Name:      containerName,
				ValueFrom: variable.ValueFrom.DeepCopy(),
			})
			if compiled.EnvironmentFrom == nil {
				compiled.EnvironmentFrom = make(map[string]string)
			}
			compiled.EnvironmentFrom[name] = containerName
		}
		services = append(services, compiled)
	}

	return services, containerEnvironment, "", ""
}

// resolveWorkspaceServices compiles services and checks that each referenced
// Secret and ConfigMap exists, so a missing one is reported on the Workspace
// rather than as a runtime container that cannot start.
func (r *WorkspaceReconciler) resolveWorkspaceServices(ctx context.Context, workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace) (string, string, error) {
	services, environment, reason, message := compileWorkspaceServices(workspace)
	if reason != "" {
		return reason, message, nil
	}
	for _, variable := range environment {
		var object client.Object
		var name, notFoundReason, kind string
		switch source := variable.ValueFrom; {
		case source.SecretKeyRef != nil:
			if source.SecretKeyRef.Optional != nil && *source.SecretKeyRef.Optional {
				continue
			}
			object, name, notFoundReason, kind = new(corev1.Secret), source.SecretKeyRef.Name, "SecretNotFound", "Secret"
		default:
			if source.ConfigMapKeyRef.Optional != nil && *source.ConfigMapKeyRef.Optional {
				continue
			}
			object, name, notFoundReason, kind = new(corev1.ConfigMap), source.ConfigMapKeyRef.Name, "ConfigMapNotFound", "ConfigMap"
		}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: workspace.Namespace}, object); err != nil {
			if errors.IsNotFound(err) {
				return notFoundReason, fmt.Sprintf("%s %s referenced by a service variable does not exist", kind, name), nil
			}
			return "", "", fmt.Errorf("get Workspace service %s: %w", kind, err)
		}
	}
	resolved.services, resolved.serviceEnvironment = services, environment

	return "", "", nil
}
