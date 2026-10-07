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

## Prune: execution history backlog

The rc controller deletes terminal WorkspaceExec history. It applies the
target's `spec.executionRetention` policy with fresh reads and delete
preconditions, in batches of at most 20 records per pass. `rcctl prune` does not
delete anything. It reports, for each Workspace and WorkspaceEnvironment:

| Column | Meaning |
| --- | --- |
| `RETENTION` | `Configured`, `Unset` (policy omitted: history is kept), `Temporary` and `TargetDeleting` (history goes with the target), `TargetMissing`, or `Unknown` (target kind not visible) |
| `RETAINED` | Records that the policy keeps, including running and pinned (`spec.retain`) records |
| `PENDING-CLEANUP` | Terminal records past the policy that wait for the controller |
| `DELETING` | Records that already have a deletion timestamp |
| `POLICY` (wide) | `ttlAfterFinished` and `maxEntries` from the target spec |

`PENDING-CLEANUP` comes from `executionretention.Build`, the same rule the
controller uses. A value that stays above zero means the controller is behind
or not running. To remove history sooner, lower
`spec.executionRetention.ttlAfterFinished` or `maxEntries` on the target. To
keep one record, set `spec.retain: true` on the WorkspaceExec.

The report issues three LIST requests (Workspace, WorkspaceEnvironment,
WorkspaceExec) and no other requests. If WorkspaceExec cannot be listed, the
report contains no targets and marks the kind as unknown.

The earlier client-side deletion flow (`--dry-run`, `--yes`, `--from-plan`,
`--history-for` and saved cleanup plans) is removed. It could only delete a
subset of what the controller deletes under the same policy.

## Verification

```sh
go test ./internal/audit ./internal/cli/rcctl/... -count=1
make lint
```

`TestExecutionHistoryReportsBacklogWithoutWrites` uses a client that fails the
test on any write. The CLI tests use a local HTTP fixture that rejects every
non-GET request. No test uses a real cluster.
