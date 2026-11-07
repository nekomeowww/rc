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

package execution

import "strings"

// KubernetesEnvironmentPrefix starts the names of the variables that the
// kubelet sets in every container for the API server Service, such as
// KUBERNETES_SERVICE_HOST. In-cluster clients use them together with the
// mounted Service Account token.
const KubernetesEnvironmentPrefix = "KUBERNETES_"

// HideKubernetesEnvironmentFlag is the rc-kube flag that the controller sets
// when a Workspace runtime has no Service Account token. rc-kube then removes
// every KubernetesEnvironmentPrefix variable from its own environment before
// it starts any process.
const HideKubernetesEnvironmentFlag = "hide-kubernetes-environment"

// IsKubernetesEnvironment reports whether name is a variable that the kubelet
// sets for the API server Service. The match ignores case, like the Windows
// environment.
func IsKubernetesEnvironment(name string) bool {
	return strings.HasPrefix(strings.ToUpper(name), KubernetesEnvironmentPrefix)
}
