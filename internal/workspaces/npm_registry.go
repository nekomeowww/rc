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
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	NPMRegistryEnvironmentName      = "NPM_CONFIG_REGISTRY"
	CorepackRegistryEnvironmentName = "COREPACK_NPM_REGISTRY"
)

// NPMRegistryEnvironment validates an npm registry URL and expands it into the
// environment variables understood by npm-compatible clients and Corepack.
func NPMRegistryEnvironment(value string) ([]corev1.EnvVar, error) {
	if value == "" {
		return nil, nil
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse --npm-registry: invalid URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("parse --npm-registry: scheme must be http or https")
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("parse --npm-registry: host is required")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("parse --npm-registry: userinfo is not allowed")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "?") {
		return nil, fmt.Errorf("parse --npm-registry: query is not allowed")
	}
	if parsed.Fragment != "" || strings.Contains(value, "#") {
		return nil, fmt.Errorf("parse --npm-registry: fragment is not allowed")
	}

	corepackURL := strings.TrimRight(value, "/")
	return []corev1.EnvVar{
		{Name: NPMRegistryEnvironmentName, Value: corepackURL + "/"},
		{Name: CorepackRegistryEnvironmentName, Value: corepackURL},
	}, nil
}

// ValidateNPMRegistryEnvironmentConflicts rejects explicit --env and
// --env-file definitions of registry variables supplied by --npm-registry.
func ValidateNPMRegistryEnvironmentConflicts(files []map[string]string, explicit []string, registry []corev1.EnvVar) error {
	if len(registry) == 0 {
		return nil
	}

	for _, variable := range registry {
		for _, file := range files {
			if _, exists := file[variable.Name]; exists {
				return fmt.Errorf("--npm-registry conflicts with --env-file value %s", variable.Name)
			}
		}
		for _, entry := range explicit {
			name, _, _ := strings.Cut(entry, "=")
			if name == variable.Name {
				return fmt.Errorf("--npm-registry conflicts with --env value %s", variable.Name)
			}
		}
	}
	return nil
}

// AddNPMRegistryEnvironment adds registry variables to a process environment,
// overriding values inherited from the rcctl caller.
func AddNPMRegistryEnvironment(values map[string]string, registry []corev1.EnvVar) {
	for _, variable := range registry {
		values[variable.Name] = variable.Value
	}
}

// RemoveNPMRegistryEnvironment removes the exact registry names from a
// process environment when a temporary Workspace owns those defaults.
func RemoveNPMRegistryEnvironment(values map[string]string) {
	delete(values, NPMRegistryEnvironmentName)
	delete(values, CorepackRegistryEnvironmentName)
}
