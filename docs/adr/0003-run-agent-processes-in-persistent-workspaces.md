---
status: accepted
---

# Run agent processes in persistent Workspaces

rc will run commands inside long-lived, namespaced Workspaces instead of
creating one Pod per command. A Workspace represents one development machine:
it owns persistent environment state, mounts one or more Worktrees, and hosts
concurrent at-most-once Agent Processes under an `rc-kube` supervisor. Client
disconnects do not stop a process, while loss of the Workspace runtime marks
its live processes as lost rather than starting them again.

Workspace Environments provide reusable development state through a committed
current volume and a mutable draft volume. Creating a Workspace clones the
current environment state and snapshots its runner image string; committing a
later Environment revision does not modify existing Workspaces. PVC cloning is
a required storage capability and never falls back to a copy Job. Repository
parent volumes are not writable Workspace code mounts. A writable mount always
uses a Worktree, and one Worktree may be mounted read-write by only one running
Workspace at a time.

This design keeps concurrent agents on the same machine when they intentionally
share files and Agent homes, preserves processes across terminal detach, and
avoids treating mutable Repository parents as working directories. It also
accepts several operational constraints: StorageClasses must support PVC
cloning, changing a Workspace volume topology requires replacing its runtime
Pod, and a replaced or lost runtime cannot resume its former processes.

Workspace access to the Kubernetes API uses an ordinary same-namespace Service
Account. rc provides one shared `rc-workspace` Service Account per namespace by
default, permits callers to disable token mounting, and permits an explicitly
selected same-namespace Service Account when broader or narrower access is
required.
