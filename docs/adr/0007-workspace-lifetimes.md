---
status: accepted
---

# ADR 0007: Minimal Workspace lifetime model

T-669 replaces T-662. Unnamed command runs are temporary; named runs and
`workspace create` retain their state. Kubernetes API creation still defaults
to `Retain`. Suspended storage deletion requires an explicit positive timeout.

## Creation intent

| Invocation | Retention |
| --- | --- |
| `workspace create dev` | `Retain` |
| `run -- COMMAND` | `DeleteAfterProcessesExit` |
| `run --name dev -- COMMAND` | `Retain` |
| `run --retain -- COMMAND` | `Retain` |
| `run [--name dev] --rm -- COMMAND` | `DeleteAfterProcessesExit` |
| `run [--name dev] --rm=false -- COMMAND` | `Retain` |
| `run --rm --retain -- COMMAND` | Error before connecting |
| `exec dev -- COMMAND` | Existing policy unchanged |

The CLI translates retention flags into one `WorkspaceRetentionPolicy` in
`RunRequest`. An empty policy lets the Runner choose by name; explicit `Retain`
or `DeleteAfterProcessesExit` overrides that default. `--rm=false` is a CLI
compatibility form, with no extra Runner booleans or API states.

Scripts that reuse an unnamed Workspace or read its logs after the cleanup
window must add `--retain`. Named scripts retain their existing behavior.
`-d` only detaches streams. The CLI prints effective retention on stderr so
command stdout remains available for its own output.

Temporary cleanup retains the existing five-minute terminal grace period and
15-minute abandoned-creation timeout. Empty, Pending, and Running phases block
cleanup; terminal executions without completion timestamps also postpone it.
Generated Worktrees and home PVCs follow Workspace ownership. Independently
supplied Worktrees are not owned or deleted. Physical PV disposal follows the
StorageClass reclaim policy; Darwin storage keeps its platform cleanup behavior.

## Two explicit clocks

```yaml
spec:
  retentionPolicy: Retain
  idleTimeout: 1h
  deleteAfterSuspended: 168h
```

`spec.idleTimeout` remains the only idle suspension clock. The only new spec
field is `deleteAfterSuspended`. Both use zero or omission to mean disabled;
negative durations are rejected. There is no lifecycle-policy wrapper, fallback
resolver, Environment lifecycle default, or dynamic inheritance. `run` and
`workspace create` expose `--idle-timeout` and `--delete-after-suspended` directly.
Direct API clients have the same defaults and fields as CLI-created Workspaces.

`Retain` prevents command-completion cleanup; it does not override explicit
suspended deletion. Temporary command cleanup takes precedence over idle stages.

The independent retention controller measures idle time from creation, even
when a Workspace never becomes Ready or has no executions. Ready, resume, and
observed execution completion advance `status.lastActivityTime`. This durable
watermark prevents removing execution history or restarting the controller from
moving the observed idle origin backward. Pending and running executions block
automatic transitions. Attaching to old output does not renew activity.

The runtime controller rechecks active executions before stopping compute.
`status.suspendedAt` records confirmed suspension; a stop request or `Ready=False`
alone is insufficient. A missing dependency can delay confirmation and therefore
storage deletion. Fixing unrelated runtime/finalization paths is outside this task.

Deletion requires a positive timeout, confirmed Suspended status for the current
generation, and no active execution. Older Suspended objects without a trustworthy
timestamp receive a complete recovery interval. Resume clears the clock; a new
confirmed suspension starts a new one. The interval is the recovery grace period,
with no automatic snapshot. Deletion remains disabled by default for existing
and newly created Workspaces.

## Execution admission and automatic deletion

A child WorkspaceExec CREATE does not change its parent's resourceVersion.
A final execution LIST followed by parent DELETE therefore cannot alone protect
a command created between those operations. The controller must serialize
runtime admission with automatic deletion on the same Workspace.

The only admission state is `status.executionAdmissionClosed`. It has no TTL.
There is no last-admitted UID: execution CRs and their existing finalizers are the
authoritative active set, so such a diagnostic UID adds no decision information.

1. Persist the WorkspaceExec and its finalizer before admission.
2. Immediately before `Runtime.Start`, read the parent without the informer
   cache, check its UID and open gate, then perform a conditional status Update.
   Keep this Update even though status is unchanged: its resourceVersion check
   is the admission linearization point. An existing owner reference pins the
   parent incarnation; direct API submissions gain ownership after admission.
3. Automatic deletion conditionally closes the gate, confirms the same closed
   UID/resourceVersion through a fresh API read, then lists executions without
   the cache. Any nonterminal execution cancels deletion.
4. DELETE uses that closed UID/resourceVersion without refreshing it after the
   LIST. Cancellation or failure reopens admission, invalidating prepared DELETE
   snapshots. A successful DELETE leaves deletionTimestamp or NotFound, so the
   gate cannot reopen.

If admission wins, its execution already exists before closure and appears in
the subsequent active scan. A successful no-op admission need not advance the
parent resourceVersion; safety comes from the persisted execution in that scan.
If closure wins, admission sees the closed gate or its stale status Update
conflicts, so `Runtime.Start` cannot run. A late direct API CR is an unadmitted
submission and receives `WorkspaceAdmissionRejected`.

ProcessClient checks fenced/deleting owners for early feedback, but controller
admission provides the protection for all clients. A parent UID/resourceVersion
precondition also protects concurrent resume, policy edits, and replacement.
A restart re-evaluates a persisted closed gate: finish expired deletion or reopen
on cancellation. A failed fence readback, including an older CRD pruning the
field, stops deletion.

Install generated CRDs before running the new controllers, and upgrade runtime
admission and retention controllers together. Explicit administrator DELETE
retains forced-deletion semantics.

## Verification scope

Runner owns one four-quadrant matrix: unnamed/named defaults and their opposite
explicit retention policies. CLI tests cover flag-to-policy conversion without
constructing another Runner. The admission package owns the CAS unit tests;
controller tests cover policy clocks and the actual deletion/runtime boundary.
One real API CAS integration covers unchanged status Update conflicts, the
winning-admission active scan, and DELETE rejection after reopening. A separate
API schema case checks disabled defaults and duration validation.

Verified locally on 2026-10-07:

- `make manifests generate` regenerated the CRD and DeepCopy code.
- `make lint-fix` and `make lint` passed with 0 issues.
- `GOFLAGS=-count=1 USE_EXISTING_CLUSTER=false KUBECONFIG=/dev/null make test-integration`
  passed all controller packages against isolated envtest Kubernetes 1.36.2.
- `GOFLAGS=-count=1 USE_EXISTING_CLUSTER=false KUBECONFIG=/dev/null make test`
  passed the full ordinary suite without cached test results.
- `git diff --check` passed.

All changes remain uncommitted in the specified worktree at baseline
`51d9ce86f84d9a704e4a23fde3546712589d9347`. No deployment or existing cluster
operation was performed.
