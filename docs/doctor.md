# Doctor

`rcctl doctor` is read-only. It never deletes, updates or patches any object.

## Commands

```sh
rcctl -n development doctor
rcctl doctor -A -o json
```

It supports table, wide, JSON and YAML output, and a namespace or `-A`. Warnings and hints go to stderr; structured stdout contains exactly one
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

Execution history is not a doctor or cleanup concern: each Workspace's
supervisor keeps and prunes it, and `rcctl ps -a` lists it. See
[`design/workspace-exec-history.md`](design/workspace-exec-history.md).

## Verification

```sh
go test ./internal/audit ./internal/cli/rcctl/... -count=1
make lint
```

The CLI tests use a local HTTP fixture that rejects every non-GET request. No
test uses a real cluster.
