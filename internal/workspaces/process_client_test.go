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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/require"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestResultErrorRejectsEveryNonSuccessTerminalPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []workspacesv1alpha1.WorkspaceExecPhase{
		workspacesv1alpha1.WorkspaceExecPhaseFailed,
		workspacesv1alpha1.WorkspaceExecPhaseStopped,
		workspacesv1alpha1.WorkspaceExecPhaseLost,
	} {
		process := &workspacesv1alpha1.WorkspaceExec{Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: phase}}
		require.Error(t, ResultError(process), "phase %s must fail a foreground command", phase)
	}
	require.NoError(t, ResultError(&workspacesv1alpha1.WorkspaceExec{Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseSucceeded}}))
}

func TestLogVolumeReadsRecordedClaims(t *testing.T) {
	t.Parallel()
	const resourceName = "demo"
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: metav1.NamespaceDefault}, Status: workspacesv1alpha1.WorkspaceStatus{HomeVolumeClaimName: "recorded-home", RuntimeImage: "runner:test"}}
	environment := &workspacesv1alpha1.WorkspaceEnvironment{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: metav1.NamespaceDefault}, Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{CurrentVolumeClaimName: "recorded-current", DraftVolumeClaimName: "recorded-draft"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace, environment).Build()
	processes := &ProcessClient{Kube: kube}
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: resourceName}}}
	volume, err := processes.logVolume(t.Context(), process)
	require.NoError(t, err)
	require.Equal(t, "recorded-home", volume.claim)
	process.Spec.TargetRef.Kind = workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment
	volume, err = processes.logVolume(t.Context(), process)
	require.NoError(t, err)
	require.Equal(t, "recorded-draft", volume.claim)
	environment.Status.DraftVolumeClaimName = ""
	require.NoError(t, kube.Update(t.Context(), environment))
	volume, err = processes.logVolume(t.Context(), process)
	require.NoError(t, err)
	require.Equal(t, "recorded-current", volume.claim)
}
