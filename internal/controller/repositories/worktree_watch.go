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

	coordinationv1 "k8s.io/api/coordination/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

func worktreeNamesForLease(ctx context.Context, kubeClient client.Client, object client.Object) []string {
	lease, ok := object.(*coordinationv1.Lease)
	if !ok {
		return nil
	}
	worktrees := new(repositoriesv1alpha1.WorktreeList)
	if err := kubeClient.List(ctx, worktrees, client.InNamespace(lease.Namespace)); err != nil {
		return nil
	}
	names := make([]string, 0, 1)
	for index := range worktrees.Items {
		worktree := &worktrees.Items[index]
		if worktreeclaim.LeaseName(worktree) == lease.Name {
			names = append(names, worktree.Name)
		}
	}
	return names
}
