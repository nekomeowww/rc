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
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	testExecutorName = "dev"
	testKVMResource  = "example.com/kvm"
	testNeedyImage   = "example.com/needy:1"
	testKVMGroup     = int64(104)
)

func needyTestConfig() NeedyConfig {
	return NeedyConfig{
		Image:           testNeedyImage,
		KVMResource:     testKVMResource,
		KVMNodeSelector: labels.Set{testKVMResource: testTrueValue},
		KVMTolerations: []corev1.Toleration{
			{
				Key:      testKVMResource,
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		},
		KVMGroup: testKVMGroup,
	}
}
func needyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))

	return scheme
}
func executorWorkspace() *workspacesv1alpha1.Workspace {
	return &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testExecutorName, Namespace: testNamespace, UID: "dev-uid"},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			Needy: &workspacesv1alpha1.WorkspaceNeedy{Executor: &workspacesv1alpha1.WorkspaceNeedyExecutor{}},
		},
	}
}
