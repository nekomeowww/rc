# WorkspaceExec and transcript retention (T-667)

This design replaces T-663's uncommitted implementation at baseline
`51d9ce86f84d9a704e4a23fde3546712589d9347`. The worktree and branch were verified
before editing. Nothing is committed, deployed, or applied to ihome.

## Contract

Retention is opt-in on each Workspace or WorkspaceEnvironment. Omission means
no automatic cleanup; `{}` enables the template below. Policies are not inherited.

```yaml
spec:
  executionRetention:
    ttlAfterFinished: 2160h
    maxEntries: 3000
    transcriptTTL: 336h
```

Terminal records (Succeeded, Failed, Stopped, Lost) expire by completion age OR
count. Pins (`WorkspaceExec.spec.retain: true`) exempt both record and transcript
from automatic cleanup and do not consume the count. Missing completion times
are conservatively initialized when observed; creation time is not completion.
CR deletion still eventually removes its transcript, even when the transcript
TTL is longer. Fourteen days is an upper bound, not a separate archive promise.
Explicit deletion overrides pins. Temporary Workspaces retain their existing
whole-target lifecycle and five-minute result-reading grace.

## Root cause and boundary

Execution control objects, diagnostic output, at-most-once identity tombstones,
and target idle clocks have different lifetimes. The deeper storage mistake is
that transcripts live under `.rc/processes` in the **cloneable home PVC**.
Environment commits and home cloning can duplicate output independently of its
CR. Per-execution finalizers cannot collect every copy or make an immutable
snapshot forget bytes. A future storage design should put diagnostics outside
cloneable home, with an independent target-scoped store and lifecycle.

This change keeps the current layout and original-PVC identity for compatibility.
Runner scope markers remove inherited transcript copies before a new target
starts; same-scope restarts preserve logs. Unmarked legacy volumes are adopted,
not blindly swept. Old orphans, storage snapshots, retained backing volumes and
external backups require a separate inventory/migration. Ownership tombstones
remain to fence delayed Start requests; this does not bound their inode count.

## Two layers and typed entry points

`WorkspaceRetentionReconciler` and `WorkspaceEnvironmentRetentionReconciler`
read their own resource kind and call one shared execution retention service.
Requests always contain real Kubernetes names; no `environment/` name encoding.
Kind-filtered execution watches and target-owned worker watches route events.

The CR planner is pure age/count selection. Its controller layer only installs
legacy finalizers and requests CR deletion. APIReader revalidates identity,
phase, pin, completion, current target policy and count peers before each write;
UID/resourceVersion preconditions fence replacement and concurrent mutation.
Count revalidation excludes records owned by a previous same-name target.
Conflicts defer one item without starving siblings. At most 20 outstanding
policy deletions are admitted per target.

The separate transcript service handles storage expiry and persisted cleanup
obligations, including when automatic retention is disabled. It gathers up to
20 entries per pass, using fresh reads. The WorkspaceExec reconciler stops an
explicitly deleted active execution, then records cleanup intent and waits;
it does not create workers or perform filesystem cleanup. Before CR removal it
preserves the target's monotonic idle clock.

## Target-owned batch cleanup

There is at most **one offline worker Pod per target UID**, shared across
executions. An immutable batch contains only execution names/UIDs and original
PVC identity. Batches sharing a PVC run together; old Environment draft volumes
are processed sequentially. The worker is controller-owned by the stable
Workspace/Environment, never by the deleting execution, and never ownerless.
It has no API token and uses existing platform/placement/security rules.

A running supervisor mounting the original volume handles pruning through its
process lock; active writers and UID mismatches fail closed. Offline batches
use the same UID-checked file deletion, preserve tombstones, and retain successful
Pod status until all CR acknowledgements are durable. Failed workers are deleted
and retried idempotently; one fixed worker name prevents duplicates after restart.
UID/resourceVersion checks protect acknowledgements. Early entries are re-read
at batch dispatch, including pins changed while later entries were assembled.
There is no transaction across policy, execution, Pod and filesystem: a pin
accepted after the final eligibility read cannot undo an operation already
in flight, just as it cannot cancel an accepted CR deletion.

Target deletion is a whole-storage lifecycle: finalizers stop waiting for per-log
cleanup and target-owned workers are garbage-collected. Missing, deleting, or
replaced PVCs also end per-transcript work; replacement data is never cleaned.
No new worker is scheduled for a deleting target/PVC. This avoids foreground GC
cycles: execution deletion preserves the target's worker; target deletion does
not depend on that worker completing or on individual transcript removal.

## One cleanup state

The single `TranscriptCleanup` Condition is the cleanup protocol:

- Absent: no cleanup obligation.
- False / Requested or CleanupFailed: outstanding intent, safe to retry.
- True / CleanupComplete or StorageGone: obligation discharged.

Condition transition time supplies the user-visible removal acknowledgement.
There are no parallel Requested booleans or DeletedAt timestamps. Only original
PVC name/UID remain as additional storage identity. Logs read the Condition and
the pinned original volume; they do not guess a new draft after an Environment
commit. Darwin node-local homes still need their live runtime for individual
pruning; whole-target deletion follows the existing target storage lifecycle.

## Reduction and test responsibilities

Before redesign: CR policy/controller 63 + 244 lines, per-execution storage
controller 302 lines, fake concurrency tests 260 lines with a 24-case Cartesian
matrix, helper tests 203 lines, real GC tests 176 lines, synthetic fake-list
benchmark 48 lines. The benchmark is deleted: fake-client allocation/time is not
API latency, informer RSS, or storage cleanup evidence. The old helper comments
and legacy owner-detachment tests described a superseded ownership model; they
are replaced by stable-target batch ownership and lifecycle tests.

Keep focused responsibilities:

- Pure policy tests: opt-in, 90d/3000, boundaries, pins, phases, deterministic count.
- Controller tests: core concurrent changes once on Workspace, one Environment
  smoke, stale API cache, conflict isolation and post-finalizer/count revalidation.
- Storage tests: batch size/ownership, retry and acknowledgement loss, pin/PVC
  identity, target/PVC deletion, actual filesystem cleanup and delayed-start fence.
- Real foreground GC: dedicated Kind cluster with a collected sentinel proving
  kube-controller-manager ran. Verify execution deletion preserves the worker,
  target deletion collects it and releases execution finalizers. Pod status is
  driven through the real API; separate file tests verify removal semantics.
- CRD envtest: opt-in defaults, validation, selectable fields. Envtest alone is
  not evidence for garbage collection.

Measured physical source lines (including comments and blanks) in the focused
retention files, before editing versus the final implementation:

| Responsibility | T-663 before | T-667 after |
| --- | ---: | ---: |
| CR planner/controller | 307 | 248 |
| Shared service and typed Environment entry | 0 | 113 |
| Transcript state, storage service and worker | 302 | 507 |
| Concurrency tests | 260 | 228 |
| Storage controller tests | 203 | 301 |
| Real foreground GC tests | 176 | 170 |
| Policy, CRD validation and execution lifecycle tests | 237 | 249 |
| Synthetic fake-list benchmark | 48 | 0 |
| **Selected files total** | **1,533** | **1,816** |

This is a lifecycle simplification, not a claim of fewer total lines. The new
batch/target cancellation protocol and its tests add code. The 24-case
Workspace/Environment × CR/transcript × mutation matrix is reduced to six
shared CR-service mutation cases. One Environment smoke covers kind routing,
original PVC identity, Windows placement and stable ownership. Focused storage
races cover pins after intent and during batch assembly; stale cache, conflict,
post-finalizer and peer-count checks remain. Extra worker-identity migration
branches and their legacy owner-detachment tests are gone.

The operational reduction is from up to one ownerless Pod per execution to one
target-owned batch Pod (at most 20 entries), and from three overlapping cleanup
state representations to one Condition. The table excludes existing runtime,
CLI, API/generated files and typed Workspace/WorkspaceExec wiring. It includes
all three new transcript controller files rather than counting only deleted
code. Pre-edit inputs are preserved locally under `bin/.t667-before` for review.

## Validation (2026-10-07)

Validation is local to the specified worktree and disposable Kind cluster.
No product controller/runner deployment or ihome operation is involved.

- `make manifests generate`: passed; generated schemas contain only original
  PVC identity plus the existing Conditions array for cleanup.
- `make lint-fix` and `make lint`: passed with zero issues.
- `GOFLAGS=-count=1 make test`: passed across all ordinary packages without cache.
- `go test -race ... -count=1`: passed for Workspace controllers, rckube,
  execution transport, Workspace client, execution CLI, runner CLI and platform.
- Envtest (`TestControllers`, Kubernetes 1.36.2, `-count=1`): all four specs passed,
  including opt-in 90d/3000/14d defaults and API validation/selectable fields.
- Real foreground GC (`TestForegroundTranscriptCleanupWithGarbageCollector`,
  Kind Kubernetes 1.36.1, `-count=2`): both runs passed. Execution deletion kept
  the worker alive; target deletion collected it and released execution cleanup.
- `git diff --check`: passed. HEAD remains the supplied baseline; no commit,
  merge, or deployment was created.

Logs are retained under `bin/t667-{generation,lint-fix,lint,make-test,race,envtest,gc}.log`.
Go 1.26.1's actual binary directory was placed first in the command PATH to avoid
the existing proto shim's extra output. No global shell or tool settings changed.

Reproduce GC only on the explicitly named disposable cluster:

```sh
mkdir -p bin/t667-gc
kind create cluster --name rc-t667-gc --image kindest/node:v1.36.1 --kubeconfig bin/t667-gc/kubeconfig
kubectl --kubeconfig bin/t667-gc/kubeconfig --context kind-rc-t667-gc apply -f config/crd/bases
RC_TRANSCRIPT_GC_KUBECONFIG="$PWD/bin/t667-gc/kubeconfig" go test -tags=integration ./internal/controller/workspaces -run '^TestForegroundTranscriptCleanupWithGarbageCollector$' -count=2 -v
kind delete cluster --name rc-t667-gc --kubeconfig bin/t667-gc/kubeconfig
```

The test uses a GC-collected dependent as evidence, following Kubernetes'
[foreground deletion semantics](https://kubernetes.io/docs/concepts/architecture/garbage-collection/#foreground-deletion).
Pod phase changes are injected through the real API to isolate GC ownership;
this does not claim a runner was scheduled or a CSI volume mounted. Actual file
removal, UID protection and at-most-once fencing have independent filesystem tests.
The dedicated cluster is removed after validation; ambient kubeconfigs and other
local clusters are untouched.

CRDs, controller, CLI and runner images must be upgraded together before opt-in.
An older runner that cannot prune leaves cleanup pending; failures never erase
the CR obligation. Old unmarked clones and offline Darwin homes remain the
storage boundaries described above, not silently swept archives.
