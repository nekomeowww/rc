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

package rckube

import (
	"fmt"
	"os"
	"strings"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

// HideKubernetesEnvironment removes every variable that the kubelet set for
// the API server Service from this process. Every process that rc-kube starts
// inherits this environment: services, executions, terminals, and lifecycle
// actions. A Workspace without a Service Account token cannot use these
// values, so they only show that the process runs in Kubernetes.
//
// Values that a Workspace sets in spec.env never come from this environment.
// They reach a process in its start request or service definition, so they
// stay.
func HideKubernetesEnvironment() error {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "" || !processruntime.IsKubernetesEnvironment(name) {
			continue
		}
		if err := os.Unsetenv(name); err != nil {
			return fmt.Errorf("remove Kubernetes variable %s: %w", name, err)
		}
	}

	return nil
}
