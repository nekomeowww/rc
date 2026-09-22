# Explicit Repository sync

Status: implemented by `rcctl repo sync NAME`.

## Operation and result

Sync fetches the configured remote and resets the parent checkout to the configured
ref or remote default branch. It removes untracked files and applies the configured
submodule policy. It uses the same Credential projection as bootstrap. Arbitrary
`repo exec` commands do not receive those credentials.

Each command creates an immutable `RepositorySync` request. `--wait` defaults to
true and prints the resolved commit. `--wait=false` prints the request name.
`kubectl get repositorysync NAME -o yaml` shows the Job, captured Repository UID
and generation, commit, completion time, and `Succeeded` condition.

The controller retains the Job until it records the terminal result. Successful
requests advance Repository `lastUpdatedAt`. Terminal results survive Job cleanup
until the request expires. `spec.ttlSecondsAfterFinished` defaults to 259200
(3 days) after success or failure. Set it when creating the immutable request.
Zero allows immediate cleanup and can delete the result before a waiter reads it.
Cleanup waits for consumers to stop and releases the parent reservation first.
It deletes only the request and its owned resources, leaving Repository and
Worktree volumes intact.
Waiters observe their own request. An earlier Repository Ready condition cannot
complete a new request. Concurrent requests compete for admission without FIFO
ordering or coalescing. Each admitted request performs its own fetch.

Existing Worktree volumes, branches, and dirty files remain unchanged. A Worktree
created after sync clones the updated parent. Existing Worktrees use ordinary Git
fetch, merge, or rebase when their owners want to update them.

## Parent access

A Lease per Repository holds persistent access reservations. Atomic
`resourceVersion` updates serialize admission across controllers. Reservations
have no timeout because expiry cannot stop a Pod that still uses the volume.
Admission distinguishes no reservation, a reservation waiting for consumers to
stop, and permission to create a consumer. API errors require retry or cleanup
because a failed response does not prove that a reservation write failed.
Workspace cleanup checks for an absent Pod before dependency readiness checks.
An unavailable dependency cannot retain a stopped Workspace's reservations.

| Consumer | Reservation | Release condition |
| --- | --- | --- |
| Bootstrap, sync, or RepositoryExec | Exclusive writer | Result recorded and consumer stopped |
| New Worktree clone | Shared clone reader | Child PVC is Bound |
| Direct Workspace Repository mount | Shared mounted reader | Workspace Pod is absent |
| Existing Worktree | None on parent | Independent child volume |

Clone readers and mounted readers cannot overlap. Kubernetes requires an unused
clone source. A Bound CSI clone is a usable independent volume; pending claims
retain their source reservations. See [CSI volume cloning](https://kubernetes.io/docs/concepts/storage/volume-pvc-datasource/).
The StorageClass must support CSI cloning and provision the child without a
consumer Pod, as required by the existing Worktree bootstrap flow.

Admission rechecks Repository generation and status through the API. Job-loss and
Pod-removal checks also bypass the informer cache. An informer delay does not
prove that a recently created consumer disappeared. Existing Pods and pending
clones are checked before admitting a writer, including during controller upgrades.
All rc consumers use this protocol; manually created Kubernetes consumers must
not race rc operations on the parent PVC.

## Failure and deletion

A failed sync leaves the parent unavailable for new clones and mounts. It does
not roll back a partial checkout. A new sync request can retry with the configured
credentials. A changed Repository generation runs bootstrap after the current
reservation is released. Existing child volumes remain usable.

Deleting a running request stops its Job and waits for its Pods before releasing
the parent. The parent remains unavailable until a later sync succeeds. A Job
that disappears before its result is recorded produces `JobLost`; rc does not
silently repeat the request.

Deleting a Worktree during provisioning retains its source reservation until the
clone is Bound. A stuck CSI operation therefore blocks parent writers until the
storage problem is resolved. This avoids assuming that deleting a pending PVC
cancels an in-flight storage operation.

## Implementation and checks

- `internal/repositoryaccess`: atomic admission and consumer checks.
- `internal/controller/repositories/repositorysync_controller.go`: request lifecycle and durable results.
- `internal/controller/repositories/repository_checkout.go`: shared Git, Credential handling, and commit reporting.
- `internal/controller/repositories/repository_operation.go`: guarded parent status updates and Job/Pod cleanup ordering.
- `internal/repositories/sync.go`: request submission and waiting.

Tests cover competing writers, pending clones, stale readiness, informer delay,
Credential projection, failed-request retry, retained results, and existing
Worktree readiness. A real Git test advances a remote and preserves a dirty child.

An isolated Kind cluster with the CSI hostpath driver also verified authenticated
HTTP sync, new and dirty existing Worktrees, Job cleanup, RepositoryExec exclusion,
and a direct Workspace mount that blocks sync until suspension.
