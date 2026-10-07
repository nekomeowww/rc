---
status: accepted
---

# Isolate persistent volume identities across rc resource kinds

## Evidence

T-660 reported a Worktree followed 11 seconds later by a same-name Workspace
in one namespace. Both controllers addressed the Worktree-owned PVC. The
Workspace repeatedly reported `VolumeClaimConflict`.

On baseline `51d9ce86f84d9a704e4a23fde3546712589d9347`, the original envtest matrix in
`internal/controller/workspaces/pvc_isolation_test.go` reproduced:

| Scenario, both creation orders | Cases | Baseline |
| --- | ---: | --- |
| Same-name Repository / Worktree / Workspace | 6 | `VolumeClaimConflict` |
| Same-name Environment and each of the other kinds | 6 | Independent volumes |
| Environment current name versus another CR's name | 6 | `VolumeClaimConflict` |
| Environment draft name versus another CR's name | 6 | `DraftVolumeMismatch` when draft is second; `VolumeClaimConflict` otherwise |

Each scenario uses API-assigned UIDs and reconciles the second resource twice.
The corrected isolation assertions produced 18 failures and 6 passes in 8.8
seconds. Worktree-before-Workspace failed in both baseline runs. Envtest has no
CSI provisioner: the regression boundary is distinct owned PVCs and absence of
conflict, not simulated storage provisioning or Pod readiness.

Current focused regression command (the original matrix was reduced in T-666):

```sh
go test -tags=integration ./internal/controller/workspaces -run '^TestControllers$' -ginkgo.focus='PVC isolation' -ginkgo.v -count=1
```

## Root cause

CR identity includes API kind, namespace and UID. PVC identity only includes its
namespace and name. Independently mapping several kinds to `metadata.name`
collapsed these identities. Checking ownership after creating the CR protected
the existing data but left an unhealthy resource behind.

The APIs already provided a storage-reference layer. Repository and Worktree
reconciliation nevertheless looked up the CR name. Workspace reconciliation,
status updates and runtime mounts all recomputed it. The shared Repository
checkout builder used the CR name for both bootstrap and sync. Environment
current/draft volumes used status references, but their unprefixed derived
names overlapped with arbitrary Repository, Worktree and Workspace names.
Draft resolution checked clone specification without checking ownership;
commit also lacked a draft/current ownership guard.

CLI creation paths validated individual resource inputs and created CRs without
checking their intended storage. Rejecting every same-name CR across kinds
would conceal the identity problem and prohibit legitimate independent objects.

## Decision

`internal/volumeclaim` owns naming, legacy discovery, ownership errors and CLI
preflight. New claims use:

| Volume | Name |
| --- | --- |
| Repository | `repository-<name>` |
| Worktree | `worktree-<name>` |
| Workspace home | `workspace-<name>-home` |
| Initial Environment current | `environment-<name>-current-<revision>` |
| Environment draft | `environment-<name>-draft-<revision>` |

Names remain predictable for diagnostics and preflight. Consumers must use
status references. Environment commit promotes the draft's existing name;
it does not rename its PVC.

Names longer than the PVC DNS-subdomain limit of 253 bytes are shortened and
suffixed with the first 16 hexadecimal SHA-256 digits of the complete name.
Trailing dots/hyphens are removed from the readable prefix. The algorithm uses
the standard library and follows the existing repository/Lease hashing family.
Ownership validation remains authoritative even for a hash collision or an
old CR whose raw name happens to equal a new typed name.

Selection order is:

1. Use the recorded status reference, checking its owner if the PVC exists.
2. If status is empty, recover a legacy-named PVC only when its controller UID
   matches the current CR. This covers a crash between PVC creation and status
   persistence.
3. Otherwise use the typed name and check any existing claim's owner.

Reconciliation never renames, copies or deletes a volume as a naming migration.
It keeps the recorded name if a missing PVC must be reprovisioned, matching
existing behavior. Unowned, foreign-owned and terminating claims at the selected
name produce a deterministic error. A recreated CR cannot adopt a prior UID's
claim. An occupied legacy name belonging to another object does not prevent
creating a fresh typed claim.

Recorded references are never discarded because a dependency is unavailable or
an ownership check fails. This also means an old Workspace whose controller
recorded a foreign PVC while reporting a conflict needs explicit recovery;
we cannot infer that it is safe to replace a previously recorded home. The
operator upgrade does not silently change its storage or touch live resources.

CLI Repository, Worktree, Workspace, Environment, mounted Worktree, and `run`
creation paths preflight the exact typed PVC before creating their CRs. `run`
checks every generated Worktree before creating any topology. Callers pass the
volume role and revision explicitly; Workspace callers skip home-PVC checks for
Darwin, while still checking generated Worktree PVCs. The volumeclaim package
does not depend on business CR types or runtime platforms. Preflight reports
PVC namespace/name and owner, including UID. Nested `rcctl` receives only PVC
`get` permission. A webhook is not added: cross-kind queries cannot atomically
reserve a name. Controllers repeat ownership checks, including Environment
draft mount/promotion and old-current deletion, to handle direct API clients
and races after CLI preflight.

## Validation

T-666 replaces the 24 ordered kind-pair cases and ten migration cases with two
envtest scenarios:

- Four same-name CR kinds allocate four independently owned PVCs.
- An owned legacy Workspace home has the Environment draft's derived name and
  identical clone spec. Reconciliation recovers and mounts the original home
  without a recorded reference, preserving its UID, while draft allocation
  creates a separate PVC owned by the Environment.

Pure naming tests cover cross-role separation, both current/draft derived-name
collisions (independent of creation order), and length/hash boundaries. Resolver
unit tests cover legacy recovery, arbitrary recorded references and owner UID
changes for every role. Each preflight unit case creates only the conflict PVC
for its owner role; a non-default Environment revision verifies caller control.
Caller tests cover preflight failures before CR/topology creation and Darwin's
home-only exemption. Existing tests retain coverage of access errors, foreign
Environment drafts, sync mounts, log-volume resolution and clone sources read
from status. The recorded-claim deletion regressions remain unchanged.

Original implementation validation on 2026-10-06:

- `make lint-fix`: 0 issues.
- `make test`: passed.
- `make test-integration`: passed, including manifests/generate, formatting and vet.
- Generated manifests and DeepCopy files remained unchanged.

The local proto Go shim polluted the linter's `go env` subprocess output.
Running lint with the same Go 1.26.1 installation's real `bin` directory first
in PATH resolved that tooling issue without changing repository configuration.
The final ordinary tests and affected controller integration suites were rerun
after lint cleanup and passed. No live-cluster resources were modified.


## Deletion lifecycle follow-up (2026-10-07)

Review found that Worktree finalization still fetched a PVC using the CR object
key. A Pending `worktree-<name>` claim therefore appeared absent. Finalization
released the Repository clone reservation and removed the Worktree finalizer
while CSI could still be copying the source. The earlier creation/migration
matrix stopped before deletion and did not cover this lifecycle transition.

Before the fix, noncached regression tests failed for recorded typed claims,
typed claims whose status write had not completed, and arbitrary recorded
legacy claims. An envtest scenario using real controller provisioning and an
API deletion request independently reproduced `RequeueAfter=0` for the Pending
typed claim. The raw-name legacy case already passed.

Deletion now uses the same `volumeclaim.Resolve` policy as provisioning, through
the API reader. Recorded names remain authoritative. Empty status recovers an
owned legacy or typed claim. The controller retains its reservation and finalizer
until that selected claim is Bound or absent; read and ownership-resolution
errors cannot be mistaken for absence. Tests also verify release after Bound,
repeated Pending reconciliation and a Bound typed-name decoy beside the actual
Pending recorded legacy claim.

The lifecycle audit covered Repository operation finalizers/admission,
Environment promotion and previous-current deletion, Workspace finalization,
hot-mount cleanup, temporary retention, CLI deletion and runner rollback.
Other direct PVC reads use status or the shared resolver. Cleanup paths that
operate on CRs/Pods rely on ownership and finalizers rather than reconstructing
PVC names.

Noncached regression commands:

```sh
go test ./internal/controller/repositories -run '^TestWorktreeDeletionWaitsForPendingClaim$' -count=1 -v
go test -tags=integration ./internal/controller/repositories -run '^TestControllers$' -ginkgo.focus='deletion|Pending typed clone' -count=1 -v
```

Follow-up validation passed: all four noncached unit scenarios, all five
selected deletion envtest cases, `make lint-fix` (0 issues), and `make test`.
No API or marker changed in this follow-up. ADR numbering is unchanged for
integration to reconcile.


## T-666 simplification validation (2026-10-07)

The replacement task keeps the existing identity/ownership/resolution policy and
recorded-claim deletion behavior. Preflight now takes explicit role/revision
arguments, and Darwin's home exemption lives in Workspace callers. The two
focused isolation specs also cover legacy-owned recovery, avoiding a separate
migration matrix.

Validation passed using the installed Go 1.26.1 binary directly in PATH:

```sh
go test ./internal/volumeclaim ./internal/repositories ./internal/workspaces ./internal/controller/repositories ./internal/cli/rcctl/commands/... -run 'TestNames|TestResolve|TestPreflight|TestCreationRejectsPVCConflict|TestRunner.*PVC|TestWorktreeDeletionWaitsForPendingClaim' -count=1
go test -tags=integration ./internal/controller/workspaces ./internal/controller/repositories -run '^TestControllers$' -ginkgo.focus='PVC isolation|deletion|Pending typed clone' -count=1 -v
make lint-fix
make lint
GOFLAGS=-count=1 make test
```

The focused envtest run passed both isolation specs and all five selected
Worktree deletion specs. Both lint commands reported zero issues. Ordinary
tests ran without cached results. No live-cluster operations, deployment,
commits or merges were performed; all changes remain in the designated worktree.
