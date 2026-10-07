# Workspace terminal runtime recovery

A Running Workspace owns replaceable compute, persistent home storage, and
at-most-once executions. Failed and Succeeded runtime Pods both need replacement:
a completed Pod does not complete the Workspace.

## Root cause and scope

At baseline `51d9ce86f84d9a704e4a23fde3546712589d9347`, the runtime path treated
all non-ready Pods as `Starting`. A terminal owned Pod therefore kept its name
and UID indefinitely. The execution controller also checked target readiness
before resolving the original binding for Starting/Pending executions, and
`originalProcessTarget` checked identity without checking terminal phase.

T-671 replaces the broader T-659 recovery design. The planner now handles only
terminal Pods. Existing Pending, initialization, readiness, topology replacement,
idle suspension, and retention behavior remain in their existing controllers.
Recovery runs before dependency resolution so that missing Worktrees or
Environments cannot hide a terminal runtime or block its cleanup.

## Ordered recovery

1. Read the owned runtime Pod and its bound executions through the API reader.
   Only Failed and Succeeded Pods enter `planTerminalRuntime`.
2. Persist `Ready=False` and `Degraded=True`, using `RuntimeFailed` or
   `RuntimeCompleted` and the real Pod/container exit diagnosis.
3. Wait for nonterminal executions bound to this exact Pod UID to become Lost
   through the execution controller. Unbound queued executions do not block.
4. Remove old hot mounts. Failed helpers must complete their node cleanup Pods.
5. Delete the terminal runtime with a UID precondition. Wait for actual absence,
   including any deletion finalizers; do not force deletion.
6. After the runtime and all mount/cleanup Pods are absent, release writer Leases
   and Repository reservations. The existing provisioning path reacquires claims
   and creates the replacement using retained home storage.
7. Publish Ready only when the replacement runtime and required mounts are ready;
   clear Degraded at that point (or when intentionally suspended).

A second terminal observation in the main Reconcile only requeues this recovery
path; it makes no separate recovery decision. The planner neither performs API
writes nor owns nonterminal lifecycle transitions.

## Restart convergence and execution safety

No dedicated recovery CRD fields or in-memory history are required. A controller
restart re-observes the terminal Pod, execution bindings, mount/cleanup Pods,
Leases, and Conditions. Status is written before destructive cleanup. A failed
status write leaves the runtime untouched; interruption after status persistence
or deletion resumes from the objects that remain. Direct API reads prevent
informer lag from admitting a replacement over an old mount.

The execution controller resolves runtime loss before target readiness,
credentials, or runner RPCs, including already-bound Pending/Starting executions.
It rechecks after claiming the runtime and before starting. A missing/replaced
UID or terminal original Pod becomes Lost without inventing a child exit code.
Terminal execution status cannot be overwritten by a late runtime reply, and a
Lost execution is never rebound to replacement compute. The existing supervisor
identity record remains the other at-most-once boundary.

## Deliberately omitted policy

There is no consecutive-failure counter, saturation threshold, repeated-failure
classification, retry deadline/backoff, readiness UID/timestamp, stable window,
or Pending timeout. None is needed to recognize terminal compute, preserve
execution identity, or order cleanup. The previous `runtimeRecovery` schema and
its last-failure snapshot are removed entirely. Degraded is an ordinary Condition,
not a persistent crash-loop state machine.

The diagnosis includes the Pod UID/node/phase, disruption and Pod reasons,
container/init exit codes, signals, and finish times. Succeeded with exit code 0
is reported as completed, not rewritten into an inferred runner failure. The
bounded diagnosis remains in Degraded while replacement starts, then yields to
current readiness. This is not a permanent failure archive or a log collector.
There is no deliberate delay for inspecting logs before deletion. Repeatedly
failing images or nodes receive the same terminal cleanup each time; this change
does not introduce a separate crash-loop throttling policy.

## Verification

Regressions cover Failed/Succeeded with and without bound executions, terminal
loss before target readiness, no rerun on replacement, late runtime replies,
loss between claim and start, UID-precondition conflicts, cleanup/Lease ordering,
API-reader cache lag, missing dependencies, and controller restart after failed
writes and throughout mount cleanup/deletion. Tests solely protecting backoff,
stability windows, and Pending timeout have been removed.

Validation commands:

```sh
make manifests generate
go test ./internal/controller/workspaces -count=1
make lint-fix
make lint
GOFLAGS=-count=1 make test
```
