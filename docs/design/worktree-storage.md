# Worktree clone storage planning

`internal/worktreestorage.PlanClone` is the single creation-time policy. Its inputs
are an observed source PVC, the Repository, and optional Worktree overrides. It
returns independently owned storage class, size, access modes and volume mode,
or a structured error. It performs no Kubernetes I/O and knows no driver names.

## Evidence and root cause (T-658 history)

T-658 reproduced both reported ihome failures locally against the original
controller before changing production code:

```sh
go test ./internal/controller/repositories -run TestWorktreeCloneStorageRegression -count=2 -v
```

Both runs failed identically: an RWO source created an RWX child; an explicit
50Gi request created a 50Gi PVC from a 60Gi source and reported `Provisioning`.
These tests reconcile through the real controller with a fake Kubernetes API
and inspect the persisted PVC. They do not simulate a CSI driver.

The root cause was a missing observation boundary. `effectiveWorktreeStorage`
accepted only Worktree and Repository intent, so it could neither observe the
real source class/access modes nor enforce a source capacity minimum. Its RWX
default encoded an inspection Pod requirement as if every driver supported it.
The original integration fixtures even declared Repository readiness without
creating a source PVC, hiding the missing observation.

The deeper lifecycle issue was recomputing inherited defaults for existing
children. A Repository size change could produce `VolumeClaimSpecChanged` for
an independent clone. Defaults are now resolved only before creation; subsequent
reconciles check ownership, source identity, filesystem mode and explicit
Worktree intent against the existing claim. A child may be expanded beyond its
explicit requested minimum. Existing legacy RWX claims are preserved, including
Pending ones; this change does not resize, reclassify, or delete previously
failed provisioning claims.

## Creation-time identity and restart recovery

The standards review exposed a second observation boundary: PVC creation and
Worktree status publication are separate API writes. The failure-injection test
`TestWorktreeCloneRecoversSourceAfterStatusWriteFailure` first lets the real
controller create its owned child, rejects the subsequent status patch, changes
`Repository.status.volumeClaimName`, and starts a fresh reconciler. Before the
fix, two uncached runs failed for both Pending and Bound children:
`VolumeClaimSpecChanged` replaced `Provisioning`/`Initializing`, and the wrong
new parent name was persisted into Worktree status. Further reconciles retained
that erroneous identity.

A successful child PVC write is the creation boundary. The owned child's
immutable `spec.dataSource` / `spec.dataSourceRef` is the authoritative source
record; Worktree status is a projection that can be missing or wrong. The
controller now reads both the Worktree creation record and the child through
`APIReader` before resolving any Repository, verifies
ownership by the current Worktree UID, then calls the pure
`worktreestorage.CloneSourceName` to recover the source. Both new creation and
restart recovery publish source identity through this common path. Existing
children recover even if the parent Repository was removed, and incorrect
status written by the previous implementation is corrected.

The [Kubernetes PVC API](https://kubernetes.io/docs/reference/kubernetes-api/core/persistent-volume-claim-v1/)
can populate either source-reference field from the other. Recovery accepts a
core PVC reference in either field, including an explicitly local namespace;
when both are present they must agree. Missing, malformed, conflicting or
cross-namespace references fail closed. Ownership and explicit storage overrides
remain checked before bootstrap; source recovery never adopts an unrelated PVC.

The additional tests exposed a deeper recreation problem: a Worktree with a
recorded child could silently create a different clone after that child was
lost. A separate failing regression showed that a stale pre-creation Worktree
from the manager cache could bypass the loss check even with a fresh child
lookup, so both observations now bypass that cache. A missing PVC with `status.volumeClaimName` already recorded now yields
`VolumeClaimLost`, revokes `VolumeReady`, releases the unused reservation, and
requires restoring the child or explicitly recreating the Worktree. A previously
observed source name alone is not creation evidence: waiting or rejected plans
can record it without ever creating a child, so those requests may still create.

The same recovery path handles a committed Create whose response was lost.
Pending children retain the original admission; Bound children release it by
Worktree token even if Repository identity changed. These changes neither rewrite
nor replace a surviving child. The recovery record is the source namespace/name,
not a historical PVC UID or CSI volume handle: those cannot be reconstructed
from these references. External deletion of both the child and its status before
either is observed, or replacement of a source during pending CSI capture,
remains outside the evidence/protection provided by these API fields.

## Policy

| Input | Rule |
| --- | --- |
| Source state | Must exist, be Bound, not deleting, and report positive request and capacity. Missing observations wait without creating a child. |
| Default size | `max(Repository.spec.storage.size, source.spec.resources.requests.storage, source.status.capacity.storage)` |
| Explicit size | Must be at least `max(source request, source capacity)`; reject a smaller value with `Ready=False`, reason `CloneSizeTooSmall`, and the required minimum. Do not silently increase an explicit request. |
| Default access modes | Inherit source PVC modes. rc Repository parents normally request RWO. |
| Explicit access modes | Preserve valid writable overrides, including RWX; reject read-only-only modes, unknown modes, and RWOP combined with other modes. Driver support is still required. |
| StorageClass | Inherit actual source class, not stale Repository intent. Preserve an explicit different class; it must support cloning from the source driver. |
| Volume mode | Nil means Filesystem. Reject Block: rc bootstraps and mounts a Git directory, not a raw device. |

Repository size is desired configuration, not proof of provisioned size.
The [external provisioner checks source request](https://github.com/kubernetes-csi/external-provisioner/blob/master/pkg/controller/controller.go)
before cloning; actual capacity can be larger because of allocation rounding or
completed expansion. Conversely, request can exceed capacity while expansion
is still progressing. Taking the larger observed value respects both limits.
Neither missing capacity nor an absent source falls back to Repository intent.

## Authority and lifecycle

The controller remains authoritative for direct CR creation, CLI requests, and
Workspace-generated Worktrees. It reads the Worktree and child outside the manager cache.
Only when no child or previous creation record exists does it resolve the
Repository, acquire clone admission, and read the source through `APIReader`
immediately before planning. Admission excludes rc-managed parent writers and
Repository expansion during capture; direct external Kubernetes edits remain
outside that protocol. This is not a transaction with CSI, so source changes
outside rc can still race provisioning.

A rejected plan releases its unused parent reservation. Missing observations
publish `Ready=Unknown/CloneSourceNotReady` and retry; source PVC watches also
trigger reconciliation. Valid plans become child claims through the existing
creation path, retaining clone admission until Bound. An existing child is never
resized, deleted, or reclassified to satisfy a newly computed parent default.

CLI commands submit overrides without baking in defaults. CLI waiters share the
planner's terminal reason classification, so invalid immutable requests fail
promptly instead of waiting for a PVC that cannot be created. Any future CLI
preflight should call this same pure function, while the controller must still
read current source state and revalidate. Preflight is advisory, not authority.

## Inspection and driver capability boundaries

The previous parallel-inspection rationale does not justify a global RWX
default. WorktreeExec and Workspace share one exclusive Worktree writer; concurrent
commands belong inside a Workspace. Linux hot-mount helpers already run on the
Workspace node. External inspection Pods must respect actual access-mode and
scheduling constraints; RWO does not promise cross-node access, and RWOP does
not permit concurrent Pods. Explicit RWX remains available on capable NFS/CephFS
CSI installations. A driver's protocol or StorageClass name is not sufficient
proof that it implements cloning or every access mode.

[Kubernetes permits different source and destination StorageClasses](https://kubernetes.io/docs/concepts/storage/volume-pvc-datasource/),
but the provisioner requires a compatible source CSI driver. `PlanClone` has
PVC-level inputs, not a PV/StorageClass inventory, so it does not certify driver
compatibility. A successful plan guarantees the observable storage invariants,
not provisioning success, topology availability, or backend quota.

The [CSIDriver API](https://kubernetes-csi.github.io/docs/csi-driver-object.html)
does not expose a per-class cloning/access-mode capability matrix. CSI has
capability RPCs, but those are driver-side interfaces rather than a portable
Kubernetes API for this controller. For now rc validates observable invariants,
uses source-compatible defaults and leaves explicit capabilities to the driver.
No StorageProfile CRD or hardcoded driver capability table is introduced.

## Verification

The pure planner matrix owns capacity/access-mode boundaries, observed source
state, overrides, and input/output aliasing. `source_test.go` owns the complete
`dataSource`/`dataSourceRef` syntax and identity matrix. Controller storage tests
own status-write failure recovery, Repository removal/replacement, and one each
of malformed source, owner incarnation mismatch, and explicit storage conflict.
The recovery sequence also checks admission release, stale status repair, and
refusal to recreate a lost child despite a stale manager cache. CLI tests cover
terminal planning errors and `VolumeClaimLost`.

Watch tests assert that source events use the Worktree Repository field index,
child events enqueue their owner directly, and unrelated ownership/namespaces
do not leak into results. A namespace-scoped envtest manager verifies actual
index registration and that the single PVC watch wakes a Pending child when it
binds, without a manual Reconcile call.

No ihome resource is changed by these tests. A real CSI provisioning test remains
separate from these deterministic creation-policy tests.

Historical validation before the T-670 simplification:

- `make manifests generate` succeeded.
- `make lint-fix` passed with 0 issues.
- `make test` passed; `internal/worktreestorage` has 100% statement coverage.
- `go test -tags=integration ./internal/controller/repositories` passed against
  local envtest Kubernetes 1.36.2 with `USE_EXISTING_CLUSTER=false`.

The local proto Go shim sometimes prints an agent-mode notice on stdout, which
breaks Go version/JSON parsing in tooling. Validation used the installed Go
1.26.1 binary directory first in the command-local PATH to avoid that wrapper;
no persistent tool configuration or repository build rules were changed.

Historical T-658 recovery review verification (2026-10-07):

- Uncached focused tests passed twice after the recovery changes:
  `go test ./internal/controller/repositories ./internal/worktreestorage ./internal/repositories -run 'Test(WorktreeClone|WorktreeClaimMatches|CloneSourceName|WorktreeClientWaitReturnsStorageFailure|PlanClone)' -count=2`.
- `make lint-fix` passed with 0 issues and `make test` passed; the storage
  planning/recovery package retained 100% statement coverage.
- `go test -tags=integration ./internal/controller/repositories -count=1` passed
  against the isolated local envtest API server (`USE_EXISTING_CLUSTER=false`).
- No ihome operations, commits, merges, or changes to other worktrees were made.

## T-670 simplification (2026-10-07)

The starting checkout was verified before edits: worktree
`/Users/neko/Git/github.com/nekomeowww/rc-task-worktrees/t658`, branch
`codex/t658-worktree-storage-plan`, HEAD/base
`51d9ce86f84d9a704e4a23fde3546712589d9347`, with the T-658 implementation
uncommitted. T-670 changes only watch routing, test responsibilities, and this
record; it adds no exported Go API, CRD field, or dependency. Clone planning and
the reconciliation/recovery state machine remain unchanged.

PVC events now use one handler. A Worktree controller ownerRef produces a direct
request with no List; a Repository controller ownerRef supplies the key for
`spec.repositoryRef.name` in the manager's Worktree field index. Other owners
produce no request. Repository events reuse the same indexed query. There is
no PVC-triggered Repository List, namespace-wide Worktree List, or duplicate
`.Owns(PVC)` route. The query cost follows the referenced Repository's dependents,
not all Repositories/Worktrees in the namespace. Parent PVCs already receive
Repository controller ownerRefs in the existing creation path; no new label or
persisted index is needed.

### Test responsibilities and size

Counts below use source lines (including comments/imports) and named leaf cases;
`PlanClone` also retains its separate nil-source assertion.

| Layer | Before | After | Responsibility |
| --- | --- | --- | --- |
| `worktree_storage_test.go` | 522 lines; 12 test functions / 28 cases | 233 lines; 2 test functions / 7 cases | Status-write failure for Pending/Bound children; Repository removed/replaced; one malformed source; owner incarnation mismatch; explicit storage conflict |
| `worktree_watch_test.go` | 1 simple watch case embedded above | 82 lines; 6 cases | Owner routing, namespace isolation, indexed List enforcement, zero Repository scans |
| `worktree_watch_integration_test.go` | None | 90 lines; 1 envtest case | Real manager index registration and child PVC event delivery |
| `plan_test.go` | 153 lines; 33 named cases | Unchanged | Pure capacity, access-mode, source-state, override and aliasing boundaries |
| `source_test.go` | 54 lines; 14 cases | Unchanged | All source-reference field permutations and input immutability |

Controller storage/watch tests together shrink from 522 to 405 lines, including
the new manager verification. Across these five files, the total falls from
729 to 612 lines. The old watch case is counted only once in the before total.

The controller no longer repeats source-reference permutations or the planner's
size/access-mode matrix. The separate lost-child, stale-cache, and uncommitted
source-observation tests are folded into the status-failure lifecycle sequence.
Legacy RWX preservation and stale-source repair are checked while recovering
after Repository removal/replacement. The separate lost-Create-response test is
removed because it reaches the same persisted child / unpublished creation
status state already exercised by the status-write failure; the recovery code
is unchanged. The dedicated uncached-source reader fake and direct matcher
matrix are removed to keep this file at the controller orchestration boundary.

Before the watch fix, `TestWorktreeClaimWatch` failed because every event called
List with `RepositoryList`. The test now enforces the indexed Worktree selector
at the client boundary, so a namespace scan cannot pass merely by returning the
correct eventual requests.

T-670 validation:

- Before edits, the existing focused tests passed with `-count=1`; the new watch
  regression then failed on the namespace-wide Repository scan.
- After edits, `go test ./internal/controller/repositories ./internal/worktreestorage ./internal/repositories -run 'Test(Worktree|CloneSourceName|PlanClone)' -count=1 -json`
  passed. The storage controller has 7 passing leaf cases, the watch mapper 6,
  `PlanClone` 33, and `CloneSourceName` 14.
- `make lint-fix` passed with 0 issues.
- `make test` passed; `internal/worktreestorage` remains at 100% statement coverage.
- `USE_EXISTING_CLUSTER=false KUBECONFIG=/dev/null KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.36.2-darwin-arm64" go test -tags=integration ./internal/controller/repositories -count=1`
  passed against local Kubernetes 1.36.2 envtest, including the real manager
  watch/index case. No real CSI driver or external cluster was used.
- `plan_test.go` and `source_test.go` are byte-for-byte unchanged from the starting
  uncommitted implementation. All commands used the installed Go 1.26.1 binary
  directory first in their command-local PATH.
- Logs and pre-edit snapshots are under the ignored `bin/t670-review/` directory
  in this worktree. No commits, merges, deployments, ihome operations, or changes
  to the historical T-658 task were made.
