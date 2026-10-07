---
status: accepted
---

# ADR 0006: Give generated Worktrees one Workspace lifecycle

Worktree content is an independent Git checkout, but its resource lifecycle can
be either independent or Workspace-owned. Independence of Git history and storage
is not a promise of persistence beyond the owning Workspace.

## Evidence and cause

At baseline `51d9ce8`, `run --repo` created a Workspace controller owner reference;
`workspace mount repo` only set `workspaces.rc.ayaka.io/generated-for`. Workspace
delete used that label only with `--cascade-created-worktrees`. The label also
selects deferred initialization, so it conflated provenance, bootstrap, and
cleanup authorization. The paths had no shared lifecycle policy.

The path matrix reproduced this twice: run resources carried ownership in both
delete modes; mount-repo resources had no owner and survived default deletion
with a label pointing at a missing Workspace. Independent mounts survived both.
The initial finalizer regression also failed twice: a deleting foreground owner
was still counted as a live mount, preventing its dependent from finishing.

An isolated Kind 1.36.1 test exposed a second problem: foreground GC propagated
through the Worktree to its PVC while a separate live Workspace still referenced
it. A finalizer on the Worktree alone did not protect its descendant's storage.
The test failed for foreground and passed for background before PVC protection.

## Policy

| Path | Ownership | Workspace deletion |
| --- | --- | --- |
| `run --repo` (retained or `--rm`) | Creating Workspace UID | Cascade |
| `workspace mount repo` | Mounting Workspace UID | Cascade |
| `workspace mount repo --read-only` | No Worktree created | Repository retained |
| `worktree add` | Independent | Retain |
| `run --worktree`, `workspace mount worktree` | Existing ownership unchanged | Retain independent resources |
| Legacy generated-for label without owner | Independent until explicit adoption | Retain and report |

Both generated paths install owner references and Worktree deletion protection
at creation, before the first reconcile. Mount references do not grant ownership.
Kubernetes GC is the only cascade mechanism. The CLI reports each affected
Worktree and requests background Workspace deletion using UID/resourceVersion
preconditions; it does not issue label-selected Worktree deletes.

The old `--cascade-created-worktrees` flag remains accepted with a deprecation
message. It never deletes label-only resources. No automatic legacy adoption and
no `--retain-created-worktrees` flag are introduced. This is an intentional safety
change: old scripts must explicitly adopt a legacy resource before expecting GC.
A same-name replacement Workspace cannot inherit the previous owner's UID.

## Explicit persistence transitions

- `worktree detach NAME --workspace OWNER` removes that exact owner's reference
  and the generated-for hint. The checkout becomes independent without changing
  its spec, PVC, mounts, or writer Lease. It is distinct from Git `add --detach`.
- `worktree adopt NAME --workspace OWNER` gives a live Workspace ownership of an
  independent, Ready Worktree, including a reviewed legacy checkout. It protects
  the existing PVC before adding an owner. It does not alter bootstrap mode.
- Both operations require current Ready status and reject deleting resources.
  Detach rejects another owner's UID, including a same-name replacement. Adopt
  refuses existing foreign/multiple owners. Optimistic patches reject concurrent
  changes rather than overwriting them. Perform detach before deleting the owner:
  once deletion has started, GC is not reversible.
- Use `worktree get NAME` (or `-o yaml`) to inspect ownership and provenance.
  Orphan legacy labels remain inspectable and are never silently adopted. A
  missing old Workspace is not reconstructed. An operator may explicitly adopt
  the Ready checkout into a different live Workspace after reviewing it.

For a generated Worktree mounted by another Workspace, CLI owner deletion fails
before stopping processes and explains how to detach or unmount. Direct API or
retention deletion proceeds, but Worktree and PVC cleanup wait for live consumers
and writers. This is intentional protection, not an owner/child deadlock. Such a
Worktree may already be terminating and cannot be rescued by a late detach.
Detach first when sharing storage that must outlive its original Workspace.

## Admission and teardown ordering

Owner references authorize cascading resource deletion. They do not serialize
mounts, and a finalizer on a parent does not keep GC from deleting descendants.
A Lease owned by a deleting Worktree can also disappear during foreground GC.
Repeated mount lists leave a read/check/delete race, even with an uncached reader.
The review regression inserts a direct-API Workspace mount immediately before
the finalizer patch; the old Workspace controller then created its runtime Pod
against the still-Ready, terminating Worktree.

Runtime admission now uses a resourceVersion-conditional patch on the Worktree
itself. `repositories.rc.ayaka.io/mount-holders` stores Workspace UID/name tokens;
`repositories.rc.ayaka.io/mounts-closed` is an irreversible deletion fence.
Both read-only and writable mounts reserve access before creating a runtime or
hot-mount helper. The Workspace controller rejects `deletionTimestamp` and the
storage fence, even if cached Ready conditions are still true.

Stable Ready reconciliation only re-admits current mounts idempotently; it does
not enumerate namespace Worktrees. `ReleaseExcept` receives explicit candidate
objects and rechecks their UIDs. The Workspace controller discovers candidates
during runtime teardown or a mount-generation transition. The runtime Pod records
`workspaces.rc.ayaka.io/worktree-mount-generation` only after helpers converge and
unused reservations are released. This checkpoint survives controller restarts
and intermediate status updates; a failed checkpoint retries cleanup. Reverting
a partially admitted spec advances generation again, so it cannot skip release.

1. Admission and closing the gate patch the same Worktree with optimistic locking.
   If DELETE or gate closure wins, a stale admission patch conflicts and its retry
   rejects the mount. If admission wins, its holder blocks cleanup, even before
   a Pod is visible. Same-name replacement objects cannot reuse another UID's
   reservation. Time passing cannot expire an admitted consumer.
2. Workspace teardown stops processes and helpers, verifies runtime/helper Pod
   absence through the API reader, and then releases reservations and writer
   Leases. Suspension remains able to tear down a closed mount. A controller
   restart reconciles reservations left by partial or ambiguous creates; it
   never assumes that a failed create meant no consumer exists.
3. Worktree cleanup closes the gate and waits for holders. It also retains the
   conservative Workspace reference check for existing/legacy mounts. A deleting
   Workspace blocks only while its runtime-cleanup finalizer remains. Once only
   Kubernetes `foregroundDeletion` remains, the owner no longer blocks its child.
4. The exclusive writer Lease still serializes Workspace and WorktreeExec writers.
   Cleanup acquires the deletion identity after closing mount admission and
   checks references and actual PVC-using Pods. WorktreeExec checks the direct
   Worktree state again after acquiring its Lease. Recreating a GC-deleted Lease
   cannot bypass a closed Worktree; an admitted exec's independently owned writer
   Lease continues to block cleanup. These extra reads protect existing clients;
   the shared Worktree CAS is the mount/deletion synchronization point.
5. PVCs carry rc volume protection from creation or controller upgrade/adoption.
   Foreground GC may request PVC deletion while holders remain, but rc keeps the
   guard until consumers stop. Normal Pending clones retain their source
   reservation until Bound. For already-terminating PVCs, rc releases its guard
   and waits for PVC cleanup, retaining the clone reservation until PVC absence.
6. After cleanup, rc releases the source reservation and Worktree finalizer.
   Kubernetes completes dependent and owner deletion under either GC policy.

Direct API writes to `Workspace.spec.mounts` remain supported: the specification
is desired state, and runtime access is admitted by the controller. A rejected
late mount can exist in the spec without obtaining volume access. This protocol
assumes one active controller per Workspace key, as provided by the controller
work queue and leader election. Administrators removing protocol annotations,
finalizers or active writer Leases, and arbitrary clients creating raw Pods, are
outside this cooperative controller protocol. Existing raw Pods are checked and
native PVC protection remains in place; an RBAC/admission policy is needed to
prevent arbitrary raw-Pod or metadata bypasses in an untrusted cluster.

## Direct PVC deletion

The second review regression directly deletes an owned PVC while its Worktree
remains live. Previously `EnsureVolumeProtection` skipped terminating PVCs, so
no normal reconciliation path released rc's finalizer. The PVC stayed Terminating.

Storage deletion is now handled before Repository readiness checks, so a missing
parent does not prevent cleanup. It permanently closes the same mount gate,
reports `VolumeDeleting`, and waits for admitted consumers, writers and actual
Pods. A suspended spec reference that never acquired access does not hold an
unused PVC forever. Only rc's finalizer is removed; Kubernetes/provisioner and
other controllers' finalizers are preserved.

After PVC absence, the live Worktree reports `VolumeDeleted` and keeps its fence.
It does not silently clone a new checkout or delete the independent Worktree.
An already-removed recorded PVC follows the same path. A replacement or foreign
PVC is left untouched with `VolumeDeletionFenced`. Recovery requires explicitly
reviewing and replacing the Worktree resource; clearing its fence is not a
supported retain/detach operation. This terminal state prevents a transient
Ready status from mounting unrelated replacement data.

Upgrade controllers before clients. Do not downgrade to a controller unaware
of volume protection and mount admission while their metadata is present.
The [Kubernetes API concurrency contract](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)
defines resourceVersion conflict handling. The
[GC documentation](https://kubernetes.io/docs/concepts/architecture/garbage-collection/)
and [v1.36 GC implementation](https://github.com/kubernetes/kubernetes/blob/v1.36.0/pkg/controller/garbagecollector/garbagecollector.go#L550-L608)
explain foreground propagation to descendants. Kubernetes also maintains its
[own PVC protection](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#storage-object-in-use-protection),
which rc does not remove.

## Verification

Ownership path regression command:

```sh
go test ./internal/cli/rcctl/commands/workspaces ./internal/controller/repositories \
  -run 'TestGeneratedWorktreeLifecycleMatrix|TestDeletingWorkspaceDoesNotBlockWorktreeGC' -count=2 -v
```

Review regressions (run uncached, including interceptor races):

```sh
go test ./internal/worktreeownership ./internal/controller/repositories \
  ./internal/controller/workspaces ./internal/workspaces \
  ./internal/cli/rcctl/commands/workspaces ./internal/cli/rcctl/commands/worktrees \
  -count=2
```

CLI lifecycle coverage runs each source once, with a separate deprecated-flag
warning test. Cleanup and access regressions are grouped by behavior rather than
in a review-only test file. They cover direct PVC deletion, missing parents, active consumers,
terminal no-reclone behavior, suspension after fencing, a mount inserted after
the final list, both orderings of CAS admission versus deletion, UID reuse,
explicit release candidates, Ready reconciles without Worktree lists, retry after
a failed topology checkpoint, and GC racing the deletion Lease or finalizer patch.
The real-API harness retries resourceVersion conflicts as the controller queue
would; it retains all consumer, storage-protection, and convergence assertions.

Real GC test (no storage driver, CSI cloning, runtime workload, or ihome access):

```sh
kind create cluster --name rc-t661-ownership --image kindest/node:v1.36.1 \
  --kubeconfig "$PWD/.task-evidence/kubeconfig"
kubectl --kubeconfig "$PWD/.task-evidence/kubeconfig" apply -f config/crd/bases
RC_OWNERSHIP_KUBECONFIG="$PWD/.task-evidence/kubeconfig" \
  go test -tags=ownershipgc ./internal/controller/repositories \
  -run 'TestWorkspaceOwnershipWithRealGC|TestLivePVCDeletionWithRealAPI' -count=2 -v
kind delete cluster --name rc-t661-ownership
```

The real-GC test refuses any kubeconfig whose selected context is not the isolated
T-661 Kind context. Ordinary tests cover identity, detach/adopt conflicts,
creation rollback, active writers, runtime cleanup, and legacy resources.

## Update: one hold set replaces the writer Lease

The Worktree write Lease and its deletion holder were removed. Mount admission,
writers and the deletion fence now share one hold set on the Worktree
(`repositories.rc.ayaka.io/holders`): Workspace mounts are `read` or `write`
holders, a WorktreeExec is a `write` holder, at most one writer is admitted, and
Close is the only deletion fence. WorktreeExec admission is a CAS on the same
object, closing the earlier plain-read gap. rcctl no longer creates Leases: a
mount only patches Workspace spec, and `worktree delete` checks references and
deletes. Legacy `mount-holders`/`mounts-closed` annotations and live
`rc-worktree-*` Leases are still honored for one release so an upgraded cluster
keeps every writer an older controller admitted. A single leader-elected
controller is assumed; running old and new controller replicas together is unsafe.
