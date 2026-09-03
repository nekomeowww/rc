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

package worktreebootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestContainerNameIsStableAndBounded(t *testing.T) {
	t.Parallel()
	first := ContainerName("development", "generated", types.UID("worktree-uid"))
	assert.Equal(t, first, ContainerName("another", "name", types.UID("worktree-uid")))
	assert.LessOrEqual(t, len(first), 63)
	assert.NotEqual(t, first, ContainerName("development", "generated", types.UID("other-uid")))
}

func TestEagerGeneratedWorktreeUsesBootstrapJob(t *testing.T) {
	t.Parallel()
	worktree := &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{generatedWorkspaceLabel: "workspace", EagerLabel: "true"}},
		Spec:       repositoriesv1alpha1.WorktreeSpec{Branch: "rc/workspace/repository"},
	}

	assert.False(t, Deferred(worktree), "bootstrap before disrupting an existing Workspace runtime")
}

func TestActionUsesIdempotentGeneratedCheckout(t *testing.T) {
	t.Parallel()
	action := Action("rc/generated/repo", "/workspace/repo")
	assert.Equal(t, "/workspace/repo", action.WorkingDirectory)
	assert.Contains(t, action.Command, "rc/generated/repo")
	assert.Contains(t, action.Command[2], "show-ref --verify")
	assert.Contains(t, action.Command[2], "checkout -b")
}
