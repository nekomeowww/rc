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
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

type partialLogExecutor struct{}

func (partialLogExecutor) Exec(_ context.Context, _ processruntime.Target, _ []string, _ io.Reader, output io.Writer, errorOutput io.Writer, _ bool) error {
	_, _ = io.WriteString(output, "partial")
	_, _ = io.WriteString(errorOutput, "rc-kube: error: truncated")
	return errors.New("command terminated with exit code 2")
}

func TestLogsNeverFallBackAfterALiveRead(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: testInventoryNamespace, UID: "runtime-uid"}}
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "flood", Namespace: testInventoryNamespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: testInventoryWorkspace},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID)},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	processes := &ProcessClient{Kube: kube, Runtime: processruntime.NewKubeRuntime(partialLogExecutor{}), Config: &rest.Config{}}

	var output, errorOutput bytes.Buffer
	require.Error(t, processes.Logs(t.Context(), process, &output, &errorOutput), "return the live read's failure")
	require.Equal(t, "partial", output.String(), "write the live output once, never followed by an offline copy")
	require.Equal(t, "rc-kube: error: truncated", errorOutput.String(), "keep bridge errors off the output")
	pods := new(corev1.PodList)
	require.NoError(t, kube.List(t.Context(), pods))
	require.Len(t, pods.Items, 1, "create no offline reader Pod")
}
