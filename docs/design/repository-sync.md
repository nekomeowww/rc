# Explicit Repository sync

Status: proposal. This document describes missing behavior, not an available command.

## Current behavior

Inspection at `c64b463` found no `rcctl repo sync` command.
The registered commands are `clone`, `exec`, `get`, `list`, and `delete`.

The Repository controller already contains the Git synchronization procedure.
It fetches the configured remote and checks out the configured full ref or remote
default branch. It resets the parent checkout and removes untracked files,
as implemented in `repositoryBootstrapScript` in the Repository controller.

The controller names the bootstrap Job after the Repository generation.
Once that generation is ready, it returns without fetching again. Neither
elapsed time nor a remote commit changes the Kubernetes spec generation.
Deleting the completed Job is not a sync mechanism: the ready fast path
intentionally preserves the ready state after Job cleanup.

`status.lastUpdatedAt` already records successful bootstrap completion.
`repo get` and `repo list` expose this timestamp and the configured ref.
They do not expose the resolved commit or the refs available in the mirror.

`repo exec` is an explicit escape hatch for exact commands. It does not mount
Repository credentials into arbitrary programs, and it does not update the
Repository synchronization timestamp. A public `git fetch` can work through
this path, but a private sync needs credential handling and checkout policy.

## Proposed operation

Use `sync` because the operation fetches remote objects and resets the parent
checkout to the configured ref. `fetch` alone does not describe the checkout change.

Add `rcctl repo sync NAME`, with `--wait` enabled by default. It submits an
explicit synchronization request and waits for that request's terminal result.
It reuses the configured remote, ref, submodule policy, and Credential.
It does not alter existing Worktree volumes or branches.

The command must state that synchronization resets the shared parent mirror.
Worktree creation can follow a successful sync to obtain current source.
Existing Worktrees need their own fetch/rebase workflow.

## Required coordination

An extra spec token alone is insufficient. It can start a new bootstrap Job
while another operation still writes the same parent volume.

The synchronization implementation must coordinate these consumers:

| Consumer | Access | Required behavior during sync |
| --- | --- | --- |
| Bootstrap or sync Job | Parent writer | One active writer; no overlapping generations |
| RepositoryExec | Parent writer | Wait behind the same writer reservation |
| New Worktree CSI clone | Parent reader | Do not begin against an in-progress reset |
| Pending CSI clone | Parent reader | Establish when source capture is complete before allowing reset |
| Workspace Repository mount | Persistent parent reader | Account for active mounts before modifying the mirror |
| Existing Worktree | Independent child volume | Continue without content changes |

Use a shared ownership protocol for the writer reservation and clone admission.
A list-then-create check in separate reconcilers does not provide exclusion.
Document the CSI assumptions before selecting the reader-release condition.
Reject unsupported storage behavior instead of assuming that every clone is
an instantaneous filesystem snapshot.

The Worktree controller currently tests `StorageReady=True` without checking
its observed generation. A sync implementation must close that stale-status
window and coordinate already admitted readers, not only strengthen the check.

## Observable results

Associate every request with an identifier and a terminal result. Report the
resolved commit, completion time, failure reason, and owning Job. Waiters must
not accept a previous request's Ready condition. Concurrent requests must have
defined serialization or coalescing semantics.

Keep full refs distinct from local branch names. The parent can have detached
HEAD, so a remote branch named `main` does not imply a local `main` branch.
Document `HEAD` as the current cloned base for Worktree creation. Ref inspection
must report actual available refs rather than inventing local branch aliases.

## Acceptance checks

- Advance a test remote, sync, and create a Worktree at the new commit.
- Keep an existing dirty Worktree unchanged through that sync.
- Authenticate a private sync with only its configured Credential.
- Serialize sync with RepositoryExec and another sync request.
- Prevent cloning during reset and wait for an admitted source capture.
- Retry a failed sync with a new request without deleting user storage.
- Reject stale Ready results and preserve terminal results after Job cleanup.
- Run storage lifecycle checks on an isolated Kind cluster.

## Evidence locations

- `internal/cli/rcctl/commands/repositories/repositories.go`: command registration.
- `internal/controller/repositories/repository_controller.go`: bootstrap, generation, readiness, and authentication.
- `internal/controller/repositories/repositoryexec_controller.go`: exec serialization and credential omission.
- `internal/controller/repositories/worktree_controller.go`: clone admission and readiness checks.
- `api/repositories/v1alpha1/repository_types.go`: configured ref and update timestamp.
