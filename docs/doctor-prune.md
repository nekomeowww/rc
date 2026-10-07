# Doctor and the execution history report

`rcctl doctor` and `rcctl prune` are both read-only. Neither command deletes,
updates or patches any object.

## Commands

```sh
rcctl -n development doctor
rcctl doctor -A -o json
rcctl -n development prune
rcctl prune -A -o wide
rcctl prune -A -o json
```

Both commands support table, wide, JSON and YAML output, and a namespace or
`-A`. Warnings and hints go to stderr; structured stdout contains exactly one
document.

## Doctor

Doctor reports runtime/storage failures, references, owner identity, deletion
blockers and PVC requests. Requested capacity is not actual usage or reclaimed
storage. Missing permissions remain `unknown`; a successful list of another kind
can still prove that its referenced dependency is absent.

Doctor reads the decisions that the controllers publish in status. It does not
derive them again on the client:

| Finding | Source |
| --- | --- |
| `MissingStorage`, `PVCConflict` | `StorageReady` (Workspace, WorkspaceEnvironment, Repository) or `VolumeReady` (Worktree) with reason `VolumeClaimLost` or `VolumeClaimConflict` |
| `MissingRuntime`, `TerminalRuntimePod` | Workspace `Ready` with reason `RuntimeMissing` or `RuntimeTerminal`; the `Degraded` reason (`RuntimeFailed`, `RuntimeCompleted`) is in the message |
| `DeletionPending`, `DeletionBlocker` | `DeletionBlocked` (Workspace, Worktree, Repository): reason, message and the time since its last transition. Other kinds, and these kinds before the controller writes the condition, show their finalizers instead |
| `StorageUnpublished` | Status names no PVC. Doctor never predicts a PVC name from the object name |
| Execution completion time | `status.completedAt` on WorkspaceExec, WorktreeExec, RepositoryExec and RepositorySync |
| Workspace evidence `idle-suspend-at`, `delete-at`, `active-executions` | `status.lifecycle`; `none` means no automatic action is scheduled |

Doctor keeps these checks on the client, because status cannot report them:

- One existence check of the PVC and Pod named in status. If the named object
  is absent, doctor reports `MissingStorage` or `MissingRuntime` even when
  `Ready` is `True`: the status can be stale, or the controller can be down.
- `StaleStatus` when `status.observedGeneration` is lower than the generation.
- PVCs without an owner reference, and reachability from rc resources.
- `VisibilityUnknown`, `RuntimeUnknown` and `StorageUnknown` for kinds that RBAC
  hides.
- Thresholds: `LongUnhealthy`, `LargeStorageRequest`, and `ProvisioningFailure`
  from PVC Events.

## Prune: execution history backlog

The rc controller deletes terminal WorkspaceExec history. It applies the
target's `spec.executionRetention` policy with fresh reads and delete
preconditions, in batches of at most 20 records per pass. It publishes the
result on the target in `status.executionHistory` and in the
`ExecutionHistoryCompliant` condition. `rcctl prune` does not delete anything.
It shows that published status for each Workspace and WorkspaceEnvironment:

| Column | Meaning |
| --- | --- |
| `RETENTION` | `Configured`, `Unset` (policy omitted: history is kept), `Temporary` and `TargetDeleting` (history goes with the target), or `Unknown` (target kind not visible) |
| `STATUS` | `Current`, `Stale` (`status.executionHistory.observedGeneration` is lower than the target generation), `Unpublished` (no summary yet), or `NotApplicable` (temporary or deleting target) |
| `COMPLIANT` | `ExecutionHistoryCompliant` reason: `WithinPolicy`, `PolicyUnset` or `CleanupBacklog` |
| `RETAINED` | Records that the policy keeps, including running and pinned (`spec.retain`) records |
| `PENDING-CLEANUP` | Records that are expired or deleting and wait for the controller |
| `POLICY` (wide) | Effective `ttlAfterFinished` and `maxEntries` after defaulting |
| `MESSAGE` (wide) | Stale or unpublished note, and the condition message |

Prune never recomputes the backlog. When the summary is `Stale` or
`Unpublished`, the controller has not caught up with the target or is not
running; the counts are the last published values or absent. A
`PENDING-CLEANUP` value that stays above zero means the controller is behind.
To remove history sooner, lower `spec.executionRetention.ttlAfterFinished` or
`maxEntries` on the target. To keep one record, set `spec.retain: true` on the
WorkspaceExec.

The report issues two LIST requests (Workspace and WorkspaceEnvironment) and no
other requests. It does not list WorkspaceExecs. If one target kind cannot be
listed, the report marks that kind as unknown and still shows the other kind.

The earlier client-side deletion flow (`--dry-run`, `--yes`, `--from-plan`,
`--history-for` and saved cleanup plans) is removed. It could only delete a
subset of what the controller deletes under the same policy.

## Verification

```sh
go test ./internal/audit ./internal/cli/rcctl/... -count=1
make lint
```

`TestExecutionHistoryReadsPublishedStatusWithoutWrites` uses a client that fails
the test on any write. The CLI tests use a local HTTP fixture that rejects every
non-GET request. No test uses a real cluster.
