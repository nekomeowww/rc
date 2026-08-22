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

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func TestResultErrorRejectsEveryNonSuccessTerminalPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []workspacesv1alpha1.AgentProcessPhase{
		workspacesv1alpha1.AgentProcessPhaseFailed,
		workspacesv1alpha1.AgentProcessPhaseStopped,
		workspacesv1alpha1.AgentProcessPhaseLost,
	} {
		process := &workspacesv1alpha1.AgentProcess{Status: workspacesv1alpha1.AgentProcessStatus{Phase: phase}}
		require.Error(t, ResultError(process), "phase %s must fail a foreground command", phase)
	}
	require.NoError(t, ResultError(&workspacesv1alpha1.AgentProcess{Status: workspacesv1alpha1.AgentProcessStatus{Phase: workspacesv1alpha1.AgentProcessPhaseSucceeded}}))
}
