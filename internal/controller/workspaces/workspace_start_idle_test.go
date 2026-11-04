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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestIdleDeadlineWaitsForAColdStart(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning,
			IdleTimeout:  &metav1.Duration{Duration: time.Minute},
		},
		Status: workspacesv1alpha1.WorkspaceStatus{
			LastActivityTime: &metav1.Time{Time: created},
			Conditions: []metav1.Condition{
				{
					Type:               workspacesv1alpha1.WorkspaceConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             "Starting",
					LastTransitionTime: metav1.NewTime(created),
				},
			},
		},
	}

	starting := lifecycleDeadlines(workspace, workspaceProcesses{})
	require.NotNil(t, starting.idleSuspendAt)
	assert.Equal(t, created.Add(workspaceStartTimeout), *starting.idleSuspendAt, "a short idle timeout does not cut the start")

	ready := created.Add(90 * time.Second)
	workspace.Status.LastActivityTime = &metav1.Time{Time: ready}
	workspace.Status.Conditions[0].Status = metav1.ConditionTrue
	workspace.Status.Conditions[0].Reason = "WorkspaceReady"
	workspace.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ready)

	running := lifecycleDeadlines(workspace, workspaceProcesses{})
	require.NotNil(t, running.idleSuspendAt)
	assert.Equal(t, ready.Add(time.Minute), *running.idleSuspendAt, "the idle clock runs from Ready")
}

func TestIdleDeadlineKeepsALongerIdleTimeoutWhileStarting(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	workspace := &workspacesv1alpha1.Workspace{
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning,
			IdleTimeout:  &metav1.Duration{Duration: time.Hour},
		},
		Status: workspacesv1alpha1.WorkspaceStatus{
			LastActivityTime: &metav1.Time{Time: created},
		},
	}

	deadlines := lifecycleDeadlines(workspace, workspaceProcesses{})
	require.NotNil(t, deadlines.idleSuspendAt)
	assert.Equal(t, created.Add(time.Hour), *deadlines.idleSuspendAt)
}
