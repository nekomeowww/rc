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
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

type stateExecutor struct{ state processruntime.State }

func (executor stateExecutor) Exec(_ context.Context, _ processruntime.Target, _ []string, _ io.Reader, output io.Writer, _ io.Writer, _ bool) error {
	return json.NewEncoder(output).Encode(executor.state)
}

func TestRuntimeResultReadsTheSupervisor(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: testInventoryNamespace, UID: "runtime-uid"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: testInventoryNamespace},
		Status:     workspacesv1alpha1.WorkspaceExecStatus{RuntimePodName: pod.Name, RuntimePodUID: string(pod.UID)},
	}
	zero, five := int32(0), int32(5)
	for name, test := range map[string]struct {
		state processruntime.State
		known bool
		code  int32
	}{
		"Succeeded":            {state: processruntime.State{Phase: supervisorPhaseExited, ExitCode: &zero}, known: true},
		"Failed":               {state: processruntime.State{Phase: supervisorPhaseExited, ExitCode: &five}, known: true, code: 5},
		supervisorPhaseStopped: {state: processruntime.State{Phase: supervisorPhaseStopped}, known: true},
		supervisorPhaseRunning: {state: processruntime.State{Phase: supervisorPhaseRunning}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			processes := &ProcessClient{Kube: kube, Runtime: processruntime.NewKubeRuntime(stateExecutor{state: test.state})}
			known, result := processes.RuntimeResult(t.Context(), process)
			require.Equal(t, test.known, known)
			if test.code != 0 {
				var exit *ProcessExitError
				require.ErrorAs(t, result, &exit)
				assert.Equal(t, test.code, exit.Code)
			}
			if name == "Succeeded" || name == supervisorPhaseRunning {
				assert.NoError(t, result)
			}
		})
	}

	replaced := process.DeepCopy()
	replaced.Status.RuntimePodUID = "other-uid"
	processes := &ProcessClient{Kube: kube, Runtime: processruntime.NewKubeRuntime(stateExecutor{state: processruntime.State{Phase: supervisorPhaseExited, ExitCode: &zero}})}
	known, _ := processes.RuntimeResult(t.Context(), replaced)
	assert.False(t, known, "never report a result from a replacement runtime")
}
