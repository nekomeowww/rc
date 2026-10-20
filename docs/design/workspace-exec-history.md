# WorkspaceExec as active intent, history in the supervisor

Status: implemented (see [Rollout](#rollout)).

This design replaced the earlier CR-based retention. It keeps the
`spec.executionRetention` field and its 90-day / 15000-entry defaults, but moves
execution history out of the Kubernetes API.

## Problem

Each command creates one WorkspaceExec, and terminal records stay until a user
opts in to retention. A long-lived cluster accumulates thousands of terminal
records for a handful of running processes. Every record adds etcd data,
list/watch work, and `rcctl` output. Opt-in retention does not fix this,
because users want long history: the defaults are 90 days and 15000 entries. Long history and one API
object per command conflict.

Retention also needed a large protocol. Transcripts live on the home PVC, so a
CR cannot be deleted until a target-owned worker removes its transcript. The
`TranscriptCleanup` Condition, batch workers, and UID-keyed cleanup exist only
because history lives in the API server. The at-most-once `owner.uid` files are
never deleted and grow without a bound.

## What needs an API record

A durable API record is required only while a process is active:

- Start intent and admission: the record and its finalizer must exist before
  `Gate.Admit`, so closing a Workspace sees every starting process.
- Runtime binding: the original Pod name and UID detect replacement and mark a
  process Lost. Supervisor state does not survive a Pod replacement.
- The stop channel (`spec.desiredState`) and the finalizer that stops a process
  before teardown.
- Ownership of the environment Secret and credential projection.

Terminal history, exit codes after the caller returns, idle clocks, and
transcript retention do not need an API object.

## Model

**WorkspaceExec is an active process.** Start, admission, Pod binding, stop, and
finalization do not change. After a process becomes terminal, its record stays
for a result window of 10 minutes so `rcctl run` and `rcctl exec` can read the
exit code. The controller then deletes it. The number of WorkspaceExecs follows
the number of concurrent processes.

**Workspace status counts active processes.** The existing
`status.lifecycle.activeExecutions` already counts nonterminal WorkspaceExecs,
and `status.lastExecutionCompletedAt` remains the idle and temporary-Workspace
clock. No new counter is added: a monotonic start count would write Workspace
status on every start, the same write that already conflicts with admission.

**The supervisor keeps history.** rc-kube writes `record.json` next to each
transcript under the state directory:

```json
{
  "id": "process-mvft...",
  "uid": "8c1d...",
  "command": ["pnpm", "test"],
  "workingDirectory": "/workspace/app",
  "tty": false,
  "phase": "Exited",
  "exitCode": 1,
  "reason": "",
  "startedAt": "2026-10-20T01:02:03Z",
  "completedAt": "2026-10-20T01:04:05Z"
}
```

The record is replaced atomically at start, at a start failure, and at exit. It
never contains environment values or credential data. When rc-kube starts, a
record that is still Running or Starting belongs to a supervisor that no longer
exists; it becomes `Lost` with reason `SupervisorRestarted`. Inspect returns a
terminal record after a supervisor restart, so a process that exited before the
controller observed it reports its real exit code instead of Lost.

A `list` protocol action returns every record, newest first, with live state
for processes the current supervisor owns.

**`ps` asks every machine.** A Workspace is one machine and rc-kube is its
process table, so `rcctl ps` reads no WorkspaceExec. It lists the Workspaces
and WorkspaceEnvironments, asks the supervisor of each running runtime Pod for
its records in parallel, and merges them oldest start first. `--workspace`
asks one machine. A stopped machine lists nothing, and a command whose start
has not reached the supervisor is not a process yet; `rcctl inspect ID` shows
it. A running target that cannot be read becomes a warning on standard error,
not a failure. `inspect` and `logs` read the WorkspaceExec while it exists and
otherwise find the process in this inventory. Callers already need
`pods/exec` for `attach` and live `logs`, so permissions do not change.

rc is experimental, so the change keeps no compatibility path. rc-kube ships in
each target's runner image; a target on an older image cannot answer `list`
and appears only as a warning until its image is updated.

**The supervisor applies retention.** `spec.executionRetention` configures the
supervisor and is enabled by default. The controller sends the current policy to
each running supervisor at most once an hour through the `prune-history`
action, so a spec change applies without replacing the runtime Pod:

| Field | Default | Meaning |
| --- | --- | --- |
| `ttlAfterFinished` | 2160h (90 days) | Remove a terminal record, transcript, and identity file after this age |
| `maxEntries` | 15000 | Keep at most this many terminal records, newest first |
| `transcriptTTL` | 336h (14 days) | Remove a terminal transcript earlier; the record stays |

Pruning never removes an active process. Removing `owner.uid` is safe once the WorkspaceExec is gone: the
controller never starts a terminal or deleted record again, and the pruning age
far exceeds any delayed start request.

History shares the home PVC lifecycle. Deleting a Workspace deletes its history,
as it does today. Moving `.rc/processes` off the cloneable home PVC is a later,
independent change; the inherited-copy scope marker also removes inherited
records.

## What this removed

The controller no longer has the transcript batch worker, the
`TranscriptCleanup` Condition, per-CR transcript cleanup, the `executionHistory`
status, or the `ExecutionHistoryCompliant` Condition. Pins (`spec.retain`) went
with them, because a WorkspaceExec no longer outlives its result window.
`rcctl prune` only reported that status and is removed. Deleting a WorkspaceExec
stops its process if it is still running and keeps the target's idle clock; it
never removes the transcript, which stays under supervisor retention.

## Rollout

Each stage is deployable alone.

1. **Supervisor history.** `record.json`, restart recovery, the `list` action,
   and supervisor retention (#69). The protocol version stays at 4.
2. **Read history from the supervisor.** `ps` aggregates every running
   supervisor and reads no WorkspaceExec. `inspect` and `logs` fall back to the
   inventory when no WorkspaceExec matches (#70).
3. **Delete terminal records.** The controller sends `executionRetention` to
   each running supervisor, deletes terminal WorkspaceExecs after the result
   window, and the CR-based retention machinery is removed. Existing terminal
   records are deleted without migrating them into supervisor history.
