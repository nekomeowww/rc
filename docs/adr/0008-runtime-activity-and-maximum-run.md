---
status: accepted
---

# ADR 0008: Runtime activity and maximum run duration

Idle suspension only saw work started through rc: a WorkspaceExec finishing
advanced `status.lastActivityTime`. A resident service inside the runtime, such
as an agent that serves tool calls through its own connection, did real work
that rc never saw, so the Workspace suspended under it. Nothing bounded a
Workspace whose work never went idle either: a stuck or looping execution kept
compute running forever.

## Decision

Scale to zero has three layers, and rc owns the first two:

| Layer | Owner | Trigger |
| --- | --- | --- |
| Idle suspension | rc, `spec.idleTimeout` | No activity for the timeout |
| Maximum run | rc, `spec.maxRunDuration` | The run reached the duration, whatever its activity |
| Node removal | the cluster autoscaler | A node has no runtime Pods left |

### Activity is recorded inside the runtime

The runtime supervisor (`rc-kube serve`) records the last activity in memory.
Work reports itself over the existing local endpoint with
`rc-kube process touch`; starting a process also counts. The supervisor never
moves the time backward.

The retention controller asks for it only at an idle deadline, through the
same Pod exec bridge that prunes history (`rc-kube process activity`). Newer
activity advances `status.lastActivityTime` and moves the deadline. There is no
push to the Kubernetes API per unit of work, so the API cost is at most one
exec per Workspace per idle timeout, however busy the Workspace is.

A supervisor too old to answer is treated as having no signal, which keeps the
previous behavior during a rolling upgrade. The local protocol keeps its
version: an older supervisor answers the new actions with "unknown action"
rather than rejecting every request. Any other query failure keeps the
Workspace running and is retried; the maximum run bounds how long that lasts.

The record lives in memory only. A replaced runtime becomes Ready again, which
is already activity.

### The maximum run ignores activity

`status.runningSince` starts at the first Ready of a run and is cleared once
suspension is confirmed. A runtime Pod restart within a run keeps it, so
restarts cannot extend a run. Each resume starts a new run.

At `runningSince + maxRunDuration` the retention controller stops every
running WorkspaceExec of the Workspace, then sets `desiredState: Suspended`.
Stopping first is required: the runtime controller does not stop compute while
executions are active. Active executions do not postpone this deadline, unlike
every other automatic stage. `status.lifecycle.maxRunSuspendAt` publishes it.

`status.lifecycle.lastAutomaticSuspendReason` records `Idle` or
`MaxRunDuration`, so a client can tell why a Workspace stopped.

Both clocks stay disabled when zero or omitted. A Workspace that must never
stop on its own, such as dedicated capacity, simply omits both.

## Rejected options

- **A per-request activity report to the Kubernetes API**, such as an
  annotation on the Workspace. Every report changed the Workspace, triggered a
  reconcile, and notified every watcher, so the API load grew with tool calls.
- **A `coordination.k8s.io` Lease renewed per report.** It avoids reconciles
  but still writes to the API at the rate of work, and needs a credential
  inside the runtime that Workspaces do not otherwise need.
- **Relying on the maximum run alone.** An idle Workspace would keep compute
  for the whole maximum run instead of the idle timeout.
