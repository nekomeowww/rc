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

// Package worktreebootstrap defines the deferred Git initialization shared by
// Worktree and Workspace controllers.
package worktreebootstrap

import (
	"crypto/sha256"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/lifecycle"
)

const (
	containerNamePrefix     = "rc-worktree-"
	generatedWorkspaceLabel = "workspaces.rc.ayaka.io/generated-for"
)

// Deferred reports whether a generated Worktree can initialize its branch in
// the consuming Workspace runtime instead of a separate bootstrap Job.
func Deferred(worktree *repositoriesv1alpha1.Worktree) bool {
	return worktree.Labels[generatedWorkspaceLabel] != "" &&
		worktree.Spec.Branch != "" &&
		worktree.Spec.ResetBranch == "" &&
		worktree.Spec.Ref == "" &&
		!worktree.Spec.Detach &&
		!worktree.Spec.Orphan &&
		!worktree.Spec.NoCheckout &&
		!worktree.Spec.Lock
}

// ContainerName returns the stable init-container name observed by the
// Worktree controller.
func ContainerName(namespace, name string, uid types.UID) string {
	identity := string(uid)
	if identity == "" {
		identity = namespace + "/" + name
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))[:12]

	return containerNamePrefix + digest
}

// Action checks out a generated branch idempotently in a cloned Repository
// root. It is safe to repeat when a runtime Pod is replaced.
func Action(branch, workingDirectory string) lifecycle.Action {
	return lifecycle.Action{
		Command:          []string{"/bin/sh", "-ceu", generatedCheckoutScript, "worktree-bootstrap", branch},
		WorkingDirectory: workingDirectory,
	}
}

const generatedCheckoutScript = `
current="$(git -c safe.directory="$PWD" symbolic-ref --quiet --short HEAD || true)"
if [ "$current" != "$1" ]; then
  if git -c safe.directory="$PWD" show-ref --verify --quiet "refs/heads/$1"; then
    git -c safe.directory="$PWD" checkout "$1"
  else
    git -c safe.directory="$PWD" checkout -b "$1"
  fi
fi
git -c safe.directory="$PWD" rev-parse --verify HEAD
`
