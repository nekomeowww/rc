// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package repositories

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// Index dependencies by their spec reference; labels and status can be absent
// before a Worktree's first reconcile.
const worktreeRepositoryIndex = "spec.repositoryRef.name"

func worktreeRepositoryIndexValues(object client.Object) []string {
	return []string{object.(*repositoriesv1alpha1.Worktree).Spec.RepositoryRef.Name}
}

// worktreesForClaim routes child events directly to their controller owner and
// source events to indexed Repository dependents. Source changes retry creation
// even when Repository intent/readiness did not change (for example expansion).
// This is the only PVC handler: registering Owns as well would duplicate delivery.
func (r *WorktreeReconciler) worktreesForClaim(ctx context.Context, object client.Object) []ctrl.Request {
	owner := metav1.GetControllerOf(object)
	if owner == nil {
		return nil
	}
	groupVersion, err := schema.ParseGroupVersion(owner.APIVersion)
	if err != nil || groupVersion.Group != repositoriesv1alpha1.GroupVersion.Group {
		return nil
	}
	key := client.ObjectKey{Namespace: object.GetNamespace(), Name: owner.Name}
	switch owner.Kind {
	case "Worktree":
		return []ctrl.Request{{NamespacedName: key}}
	case "Repository":
		return r.worktreesForRepository(ctx, key)
	default:
		return nil
	}
}

// worktreesForRepository queries only Worktrees that reference this Repository,
// shared by Repository events and its owned source PVC events.
func (r *WorktreeReconciler) worktreesForRepository(ctx context.Context, key client.ObjectKey) []ctrl.Request {
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	if err := r.List(ctx, worktrees, client.InNamespace(key.Namespace), client.MatchingFields{worktreeRepositoryIndex: key.Name}); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(worktrees.Items))
	for _, worktree := range worktrees.Items {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&worktree)})
	}
	return requests
}
