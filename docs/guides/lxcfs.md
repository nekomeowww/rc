# LXCFS for Linux Workspaces

Without LXCFS, a Linux Workspace reads node values from `/proc` and `/sys`.
Tools such as `free`, `top`, `nproc`, and `uptime` then show the memory, CPUs,
uptime, and load of the whole node, not of the Workspace.

[LXCFS](https://github.com/lxc/lxcfs) is a FUSE file system that serves
container-aware copies of these files. When you enable LXCFS in rc, each Linux
Workspace sees these values:

- Its own memory limit in `/proc/meminfo` and `/proc/swaps`.
- A CPU count from its CFS quota in `/proc/cpuinfo`, `/proc/stat`, and
  `/sys/devices/system/cpu/online`.
- Its own uptime in `/proc/uptime`.
- Its own load average in `/proc/loadavg`.
- Its own I/O counters in `/proc/diskstats`.

## Node requirements

LXCFS must run on every node that runs Linux Workspace runtime Pods. rc does not
install or start LXCFS.

- Start LXCFS with `--enable-cfs` and `--enable-loadavg`. Without
  `--enable-cfs`, the CPU count is the node CPU count. Without
  `--enable-loadavg`, `/proc/loadavg` shows the node load.
- Serve LXCFS at one directory that is the same on all nodes, for example
  `/var/lib/lxcfs`. rc reads `<dir>/proc/<file>` and
  `<dir>/sys/devices/system/cpu/online`.
Linux runtime Pods already use `hostPath` volumes for Worktree hot mounts. Thus,
LXCFS adds no new Pod Security requirement.

rc mounts each file with the `hostPath` type `File`. If a node does not serve a
file, the kubelet does not start the Pod and reports a `FailedMount` event on
it. The Pod does not start with an empty file.

When LXCFS restarts on a node, the mounts in running containers on that node
stop working. Suspend and start the affected Workspaces to get new mounts.

## Enable LXCFS

Set the `--lxcfs-dir` manager flag to the LXCFS directory. With the kustomize
manifests, set the `RC_LXCFS_DIR` variable of the manager container:

```yaml
env:
  - name: RC_LXCFS_DIR
    value: /var/lib/lxcfs
```

An empty value disables LXCFS. This is the default.

When LXCFS is enabled, the `lxcfs-` prefix is reserved for volume names. If a
Workspace mount name starts with `lxcfs-`, the Ready reason is
`ReservedMountName`.

The flag changes only Linux Workspace runtime Pods. Windows and Darwin
Workspaces, WorkspaceEnvironment editor Pods, and helper Pods do not get these
mounts.

## Existing Workspaces

The LXCFS directory is part of the runtime topology hash of a Linux Workspace.
When the flag is empty, the hash does not change, so existing Pods stay as they
are.

When you set the flag, or change or clear it, each running Linux Workspace
follows the usual topology rules:

- If the Workspace has no active executions, rc deletes the runtime Pod and
  creates a new Pod with the new mounts. The Ready reason is `Replacing`.
- If the Workspace has active executions, rc keeps the Pod and sets the Ready
  reason to `TopologyChangeBlocked`. rc replaces the Pod after the executions
  stop.

Suspended Workspaces get the mounts when they start again. The home volume and
the Workspace state stay the same through the replacement.
