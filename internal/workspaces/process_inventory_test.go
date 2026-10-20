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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const (
	testInventoryNamespace = "development"
	testInventoryWorkspace = "coding"
)

type podResult struct {
	entries []processruntime.HistoryEntry
	err     error
}

type podHistory map[string]podResult

func (history podHistory) List(_ context.Context, target processruntime.Target) ([]processruntime.HistoryEntry, error) {
	result, ok := history[target.Pod]
	if !ok {
		return nil, errors.New("unexpected target " + target.Pod)
	}

	return result.entries, result.err
}

func historyEntry(id, phase string, exitCode *int32, started time.Time, command ...string) processruntime.HistoryEntry {
	return processruntime.HistoryEntry{
		State:     processruntime.State{ID: id, Phase: phase, ExitCode: exitCode},
		Command:   command,
		StartedAt: &started,
	}
}

func inventoryFixture(t *testing.T, history podHistory) *ProcessInventory {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	running := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testInventoryNamespace},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	pending := running("pending-pod")
	pending.Status.Phase = corev1.PodPending
	created := metav1.NewTime(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	objects := []client.Object{
		&workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: testInventoryWorkspace, Namespace: testInventoryNamespace},
			Status:     workspacesv1alpha1.WorkspaceStatus{RuntimePodName: "coding-pod"},
		},
		&workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: testInventoryNamespace},
			Status:     workspacesv1alpha1.WorkspaceStatus{RuntimePodName: "legacy-pod"},
		},
		&workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: testInventoryNamespace},
			Status:     workspacesv1alpha1.WorkspaceStatus{RuntimePodName: "broken-pod"},
		},
		&workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "suspended", Namespace: testInventoryNamespace}},
		&workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "starting", Namespace: testInventoryNamespace},
			Status:     workspacesv1alpha1.WorkspaceStatus{RuntimePodName: "pending-pod"},
		},
		&workspacesv1alpha1.WorkspaceEnvironment{
			ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: testInventoryNamespace},
			Status:     workspacesv1alpha1.WorkspaceEnvironmentStatus{EditorPodName: "editor-pod"},
		},
		&workspacesv1alpha1.WorkspaceExec{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-running", Namespace: testInventoryNamespace, CreationTimestamp: created},
			Spec: workspacesv1alpha1.WorkspaceExecSpec{
				TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: "legacy"},
				Command:   []string{"sleep", "60"},
			},
			Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseRunning},
		},
		running("coding-pod"),
		running("legacy-pod"),
		running("broken-pod"),
		running("editor-pod"),
		pending,
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithIndex(&workspacesv1alpha1.WorkspaceExec{}, "spec.targetRef.name", func(object client.Object) []string {
			return []string{object.(*workspacesv1alpha1.WorkspaceExec).Spec.TargetRef.Name}
		}).
		Build()

	return &ProcessInventory{Kube: kubeClient, History: history}
}

func TestProcessInventoryAggregatesRunningSupervisors(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	base := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	zero, one := int32(0), int32(1)
	inventory := inventoryFixture(t, podHistory{
		"coding-pod": {entries: []processruntime.HistoryEntry{
			historyEntry("process-server", "Running", nil, base.Add(2*time.Minute), "pnpm", "dev"),
			historyEntry("process-test", "Exited", &one, base, "pnpm", "test"),
			historyEntry("codex-task", "Exited", &zero, base.Add(time.Minute), "codex", "exec"),
		}},
		"editor-pod": {entries: []processruntime.HistoryEntry{
			historyEntry("process-setup", "Stopped", nil, base.Add(3*time.Minute), "sh"),
		}},
		"legacy-pod": {err: processruntime.ErrHistoryUnsupported},
		"broken-pod": {err: errors.New("container not ready")},
	})

	processes, warnings, err := inventory.List(t.Context(), InventoryRequest{Namespace: testInventoryNamespace})
	requirements.NoError(err, "one unreadable target does not fail the inventory")

	ids := make([]string, 0, len(processes))
	for _, process := range processes {
		ids = append(ids, process.ID)
	}
	assertions.Equal([]string{"legacy-running", "process-test", "codex-task", "process-server", "process-setup"}, ids, "oldest start first across machines")
	assertions.Equal("Failed", processes[1].DisplayPhase())
	assertions.Equal("Succeeded", processes[2].DisplayPhase())
	assertions.Equal("codex", processes[2].Agent, "derive the agent from the command")
	assertions.Equal(workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment, processes[4].Target.Kind)
	assertions.Equal("Running", processes[0].DisplayPhase(), "fall back to records for an old runner image")

	requirements.Len(warnings, 2)
	assertions.Equal("broken", warnings[0].Target.Name)
	assertions.Equal("legacy", warnings[1].Target.Name)
	assertions.ErrorIs(warnings[1].Err, processruntime.ErrHistoryUnsupported)

	only, _, err := inventory.List(t.Context(), InventoryRequest{Namespace: testInventoryNamespace, Workspace: testInventoryWorkspace})
	requirements.NoError(err)
	assertions.Len(only, 3, "--workspace asks only that machine")

	_, err = inventory.Find(t.Context(), testInventoryNamespace, "process-se")
	requirements.Error(err, "a shared prefix is ambiguous")
	found, err := inventory.Find(t.Context(), testInventoryNamespace, "codex")
	requirements.NoError(err)
	assertions.Equal("codex-task", found.ID)
	target, err := inventory.RuntimeTarget(t.Context(), found.Target)
	requirements.NoError(err)
	assertions.Equal("coding-pod", target.Pod)
}
