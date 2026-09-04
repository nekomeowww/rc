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

package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWorkspaceSpecEffectiveRetentionPolicy(t *testing.T) {
	t.Parallel()

	assert.Equal(t, WorkspaceRetentionPolicyRetain, (WorkspaceSpec{}).EffectiveRetentionPolicy(), "default to retaining a Workspace")
	assert.Equal(t, WorkspaceRetentionPolicyDeleteAfterProcessesExit, (WorkspaceSpec{
		RetentionPolicy: WorkspaceRetentionPolicyDeleteAfterProcessesExit,
	}).EffectiveRetentionPolicy(), "preserve an explicit temporary retention policy")
}

func TestWorkspaceSpecIsTemporary(t *testing.T) {
	t.Parallel()

	assert.False(t, (WorkspaceSpec{}).IsTemporary(), "treat the default lifetime as retained")
	assert.True(t, (WorkspaceSpec{
		RetentionPolicy: WorkspaceRetentionPolicyDeleteAfterProcessesExit,
	}).IsTemporary(), "derive temporary lifetime from the retention policy")
}
