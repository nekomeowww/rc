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

// Package worktreeclaim defines the Lease identity shared by every Worktree
// writer.
package worktreeclaim

import (
	"crypto/sha256"
	"encoding/hex"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

const (
	// HolderLabel identifies the resource holding a Worktree write Lease.
	HolderLabel = "workspaces.rc.ayaka.io/write-holder"
	// DeletionFinalizer keeps a Worktree present until the controller has
	// acquired its exclusive Lease and verified there are no Workspace mounts.
	DeletionFinalizer = "repositories.rc.ayaka.io/worktree-delete-protection"
	leasePrefix       = "rc-worktree-"
	deletePrefix      = "worktree-delete/"
)

// LeaseName returns the stable exclusive-write Lease name for a Worktree.
func LeaseName(worktree *repositoriesv1alpha1.Worktree) string {
	identity := string(worktree.UID)
	if identity == "" {
		identity = worktree.Namespace + "/" + worktree.Name
	}
	sum := sha256.Sum256([]byte(identity))

	return leasePrefix + hex.EncodeToString(sum[:10])
}

// DeletionHolder returns the reserved holder identity used while deleting a
// Worktree. Acquiring this identity through the ordinary write Lease closes
// the race between a client-side availability check and the delete request.
func DeletionHolder(worktree *repositoriesv1alpha1.Worktree) string {
	return deletePrefix + string(worktree.UID)
}

// DeletionLease builds the exclusive claim that protects Worktree deletion
// from a concurrently starting writer.
func DeletionLease(worktree *repositoriesv1alpha1.Worktree) *coordinationv1.Lease {
	holder := DeletionHolder(worktree)
	controller := true
	blockOwnerDeletion := true

	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaseName(worktree),
			Namespace: worktree.Namespace,
			Labels:    map[string]string{HolderLabel: worktree.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         repositoriesv1alpha1.SchemeGroupVersion.String(),
				Kind:               "Worktree",
				Name:               worktree.Name,
				UID:                worktree.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

// IsDeletionHolder reports whether the Lease holder belongs to the Worktree's
// server-side deletion protocol.
func IsDeletionHolder(worktree *repositoriesv1alpha1.Worktree, lease *coordinationv1.Lease) bool {
	return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == DeletionHolder(worktree)
}
