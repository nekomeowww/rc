# Operations & Custom Images

This guide covers day-2 operator workflows: upgrading Workspace images, building custom Linux images with controller-injected `rc-kube`, configuring Pod scheduling and metadata, and handling API upgrades.

## Image upgrades

A blank Workspace uses `spec.image`, or the controller's `--runner-image` when `spec.image` is empty.

- When the effective image changes while a Workspace is running, the Pod keeps its current image and reports `Outdated=True` with reason `ImageChangePending`.
- `status.runtimeImage` reports the image of the current or last runtime Pod.
- The Workspace switches to the new image on its next start.
- A Workspace created from a `WorkspaceEnvironment` retains its Environment revision's image.

```sh
kubectl patch workspace dev --type merge -p '{"spec":{"image":"ghcr.io/nekomeowww/rc/runner:<version>"}}'
rcctl workspace stop dev
rcctl workspace start dev
```

See [ADR 0011: Workspace image upgrades](../adr/0011-workspace-image-upgrades.md).

## Custom Linux images & `rc-kube` injection

On Linux, the controller injects matching `rc-kube` and `tini` binaries into every `Workspace` and `WorkspaceEnvironment` Pod via an init container from the `--rc-kube-image` (`RC_KUBE_IMAGE`, default `ghcr.io/nekomeowww/rc/rc-kube:<release>`). Both binaries are copied into `/opt/rc/bin`, and the container starts `/opt/rc/bin/rc-kube`.

If your cluster pulls through a private mirror, mirror `ghcr.io/nekomeowww/rc/rc-kube` and point `--rc-kube-image` at the mirror.

Because `rc-kube` and `tini` are injected automatically, a custom Linux image only needs:

1. The tools and runtimes your Workspace requires, plus a POSIX shell for lifecycle scripts.
2. Files accessible to UID/GID `1000` (the runtime user).
3. `rc-kube` on `PATH` only if you want `rcctl logs` to inspect a **suspended** Workspace (the log-reader helper Pod uses the Workspace image).

### Init systems and process reaping

- When `rc-kube serve` starts as Linux PID 1, it re-executes itself under the injected `tini` to reap orphaned descendants while preserving direct command exit statuses.
- Images that ship their own init system can set `RC_KUBE_INIT` to an executable; `rc-kube serve` then runs `$RC_KUBE_INIT <rc-kube path> serve <arguments>` instead of `tini` (as `runner-wayland` does).
- Processes started by `rc-kube` receive `/opt/rc/bin` first on `PATH`. Login shells that reset `PATH` can invoke `/opt/rc/bin/rc-kube` directly.
- Windows and macOS images still bundle their own `rc-kube`.

Verify orphan cleanup, exit status propagation, and supervisor shutdown in an isolated container with:

```sh
bash hack/test-runner-reaping.sh <runner-image>
```

See [ADR 0010: Inject `rc-kube` from the controller](../adr/0010-inject-rc-kube-from-the-controller.md).

## Scheduling & runtime Pod metadata

`workspace create` and `run` control Pod placement and metadata with `--node-selector`, `--toleration`, `--priority-class`, `--pod-label`, and `--pod-annotation`:

```sh
rcctl -n development workspace create dev \
  --image ghcr.io/nekomeowww/rc/runner:latest \
  --storage-class csi-hostpath-sc \
  --priority-class workspace-standard \
  --pod-label example.com/tenant=team-a \
  --pod-annotation karpenter.sh/do-not-disrupt=true
```

- Labels and annotations apply only to the Workspace runtime Pod and are visible in `rcctl workspace get`.
- Keys under `rc.ayaka.io` (or any subdomain) are reserved by `rc` and rejected.
- Updating `spec.podMetadata` patches a running Pod in place.
- Updating `spec.priorityClassName` replaces an idle runtime Pod and waits while executions are active.

## Container-aware `/proc` and `/sys` (LXCFS)

To show each Linux Workspace its own memory limit, CPU count, uptime, and load average rather than the host node's values, run LXCFS on Linux nodes and pass `--lxcfs-dir` (`RC_LXCFS_DIR`). See the [LXCFS guide](lxcfs.md).

## Diagnostics

Use read-only `rcctl doctor` to audit PVC requests, stale statuses, missing runtime Pods, and deletion blockers across a namespace or the entire cluster. See [Doctor](../doctor.md).

## Upgrading from `AgentProcess` to `WorkspaceExec`

The execution API is `workspaces.rc.ayaka.io/v1alpha1`, kind `WorkspaceExec` (replacing the legacy `AgentProcess` resource and `rcctl agent` command group). Existing `AgentProcess` records are not automatically migrated:

1. Save any needed logs, then stop and delete old process records with the previous CLI/controller.
2. Upgrade CRDs, controller, and `rcctl` together.
3. Do not relabel completed executions as new `WorkspaceExec` resources — creating a `WorkspaceExec` requests a new command execution.
