# Darwin Workspaces with macOS-vz-kubelet

rc can run an experimental Workspace inside the macOS virtual machine represented
by an [`agoda-com/macOS-vz-kubelet`](https://github.com/agoda-com/macOS-vz-kubelet)
Pod. The manager remains on Linux. `spec.os: darwin` selects the macOS virtual
kubelet node, uses guest-native paths, and talks to `rc-kube` through Pod exec.

This integration deliberately models the provider as a virtual machine, not a
Darwin container runtime:

- the first container image is a macOS VM OCI artifact;
- persistent Workspace home data is a node-local host directory shared into the
  guest with VirtioFS;
- a postStart hook starts the guest `rc-kube` supervisor; and
- the Pod omits `spec.os`, which only admits Linux and Windows, and schedules by
  `kubernetes.io/os: darwin` instead.

## Requirements

The VM image must contain an arm64 `rc-kube` binary at
`/usr/local/bin/rc-kube`. Build it with:

```sh
make build-darwin
```

Install `bin/darwin-arm64/rc-kube` into the VM image and make it executable.
The guest SSH user must be able to run that file and write the VirtioFS share at
`/Volumes/My Shared Files/home`.

The provider must support exec postStart hooks and exact Kubernetes argv. rc
sets `MACOS_VZ_KUBERNETES_EXEC_ARGV=1` in the VM container to opt into exact
argv while preserving the provider's legacy line-oriented hook behavior for
other Pods.

Configure the manager with both Darwin settings:

```yaml
env:
  - name: RC_DARWIN_RUNNER_IMAGE
    value: registry.example.test/team/macos-xcode:26.3
  - name: RC_DARWIN_WORKSPACE_ROOT
    value: /Users/runner/.local/share/rc/workspaces
```

The root is interpreted on the macOS-vz-kubelet host, not in the manager Pod.
Each Workspace receives `<root>/<namespace>/<workspace>` with
`DirectoryOrCreate`. Keep Darwin Workspaces pinned to the node that owns this
directory.

## Create and use a Workspace

For a node tainted like macOS-vz-kubelet's default:

```sh
rcctl -n rc-dev workspace create xcode \
  --os darwin \
  --image registry.example.test/team/macos-xcode:26.3 \
  --node-selector kubernetes.io/hostname=neko-macos-1 \
  --toleration virtual-kubelet.io/provider=macos-vz:NoSchedule

rcctl -n rc-dev agent exec --workspace xcode -- xcodebuild -version
rcctl -n rc-dev agent run --detach --workspace xcode -- your-gui-process
```

Service-account token mounting defaults off for Darwin. AgentCredential and
Credential data still travel through the rc process protocol and do not depend
on Kubernetes volume projection.

## Current boundaries

Darwin WorkspaceEnvironment clones, Repository/Worktree PVC mounts, ConfigMap
mounts, Secret mounts, and `rcctl workspace port-forward` are not supported.
The controller reports an explicit condition before creating a Pod when one of
these is requested. Storage capacity is governed by the macOS host filesystem;
`--size` and `--storage-class` do not apply.

Suspending and resuming deletes and recreates the VM Pod while retaining the
shared Workspace home. The VM root filesystem is still ephemeral, so install
system-wide tools and Xcode/Simulator runtimes in the base VM image. Deleting a
Workspace does not recursively delete its host directory; an administrator can
archive or remove it on the macOS host.

`rcctl agent logs` uses Pod exec while the Darwin Workspace is running. Unlike
PVC-backed runtimes, rc cannot create a lightweight transcript-reader container
after the VM Pod is suspended; resume the Workspace before reading its logs.
