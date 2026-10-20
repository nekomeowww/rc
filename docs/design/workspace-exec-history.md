# WorkspaceExec as active intent, history in the supervisor

Status: accepted; implemented in stages (see [Rollout](#rollout)).

This design supersedes the storage model of
[`workspace-exec-retention.md`](workspace-exec-retention.md). It keeps the
`spec.executionRetention` field and its 90-day / 15000-entry defaults, but moves
execution history out of the Kubernetes API.

## Problem

Each command creates one WorkspaceExec, and terminal records stay until a user
opts in to retention. On the ihome cluster, 3487 WorkspaceExecs existed while 6
were running. Every record adds etcd data, list/watch work, and `rcctl` output.
Opt-in retention does not fix this, because the user wants long history: the
defaults were raised to 90 days and 15000 entries. Long history and one API
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

**Workspace status counts processes.**

```yaml
status:
  processes:
    active: 3          # nonterminal WorkspaceExecs for this target
    started: 1284      # monotonic count of admitted starts
  lastExecutionCompletedAt: ...   # existing idle and temporary-Workspace clock
```

WorkspaceEnvironment publishes the same `processes` summary.

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
for processes the current supervisor owns. `rcctl ps -a`, `inspect`, and `logs`
read finished processes from the supervisor. When the runtime is stopped, they
use the existing offline reader on the original PVC. Callers already need
`pods/exec` for `attach` and live `logs`, so permissions do not change.

**The supervisor applies retention.** `spec.executionRetention` now configures
the supervisor and is enabled by default:

| Field | Default | Meaning |
| --- | --- | --- |
| `ttlAfterFinished` | 2160h (90 days) | Remove a terminal record, transcript, and identity file after this age |
| `maxEntries` | 15000 | Keep at most this many terminal records, newest first |
| `transcriptTTL` | 336h (14 days) | Remove a terminal transcript earlier; the record stays |

The supervisor prunes at startup and periodically. It never removes an active
process. Removing `owner.uid` is safe once the WorkspaceExec is gone: the
controller never starts a terminal or deleted record again, and the pruning age
far exceeds any delayed start request.

History shares the home PVC lifecycle. Deleting a Workspace deletes its history,
as it does today. Moving `.rc/processes` off the cloneable home PVC is a later,
independent change; the inherited-copy scope marker also removes inherited
records.

## What this removes

After the last stage, the controller no longer needs the transcript batch
worker, the `TranscriptCleanup` Condition, per-CR transcript cleanup, the
`executionHistory` status, or the `ExecutionHistoryCompliant` Condition. Pins
(`spec.retain`) are removed with them, because a WorkspaceExec no longer
outlives its result window.

## Rollout

Each stage is deployable alone.

1. **Supervisor history.** `record.json`, restart recovery, the `list` action,
   and supervisor retention. Retention is off unless `rc-kube serve` receives a
   policy, so this stage does not change behavior. The protocol version stays
   at 4: a new client sends `list` to an old supervisor and receives
   `unknown action`.
2. **Read history from the supervisor.** `ps -a`, `inspect`, and `logs` read
   finished processes from rc-kube. Workspace and WorkspaceEnvironment publish
   `status.processes`.
3. **Delete terminal records.** The controller passes `executionRetention` to
   rc-kube, deletes terminal WorkspaceExecs after the result window, and removes
   the CR-based retention machinery. Before deleting an existing terminal
   record on a running target, the controller asks rc-kube to write its
   `record.json`; records on stopped targets are deleted without history.
