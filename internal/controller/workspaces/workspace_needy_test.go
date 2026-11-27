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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/apis/kubernetes/workspaces/v1alpha1"
)

func TestWorkspaceNeedyExecutorService(t *testing.T) {
	workspace := serviceWorkspace()
	workspace.Spec.Needy = &workspacesv1alpha1.WorkspaceNeedy{Executor: &workspacesv1alpha1.WorkspaceNeedyExecutor{}}
	services, environment, reason, _ := compileWorkspaceServices(workspace)
	require.Empty(t, reason)
	require.Len(t, services, 2)
	service := services[1]
	assert.Equal(t, needyExecutorServiceName, service.Name)
	assert.Equal(t, "/opt/needy/bin/needy runner serve --address tcp://needy-daemon."+testNamespace+".svc:7400 "+
		"--name ws-"+workspace.Name+" "+
		"--credential-env "+needyCredentialVariable+" --roles executor --activity-hook /opt/rc/bin/rc-kube process touch",
		strings.Join(service.Command, " "))
	var credential *corev1.EnvVar
	for i := range environment {
		if source := environment[i].ValueFrom; source != nil && source.SecretKeyRef != nil && source.SecretKeyRef.Name == workspace.Name+"-needy-runner" {
			credential = &environment[i]
		}
	}
	require.NotNil(t, credential)
	assert.Equal(t, needyCredentialKey, credential.ValueFrom.SecretKeyRef.Key)

	workspace.Spec.Services[0].Name = needyExecutorServiceName
	_, _, reason, _ = compileWorkspaceServices(workspace)
	assert.Equal(t, reasonInvalidServiceEnvironment, reason)
}
