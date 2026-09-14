# Windows Workspaces

rc can run Windows Workspace and Environment runtimes with a Windows image.
The manager continues to run on Linux. Set `spec.os: windows`; rc selects Windows
nodes, uses Windows paths and PowerShell lifecycle scripts, and communicates
with the supervisor through a local named pipe. Linux remains the API default.

## Runtime contract

| Behavior | Windows implementation |
| --- | --- |
| Runtime image | Native `rc-kube.exe` and `rcctl.exe` on PATH; entrypoint invokes supplied argv |
| Home and transcripts | Persistent volume at `C:\home\agent` |
| Development checkout | Worktree PVC at `C:\workspace\<mount>`; unmounted paths remain container-local |
| Temporary credentials | `C:\run\rc`; removed when their owning process exits |
| Supervisor connection | Owner-restricted `\\.\pipe\rc-kube` |
| Interactive processes | ConPTY, including attach and resize |
| Process cleanup | A nested Job Object per process tree; assignment precedes execution |
| Readiness | `rc-kube.exe health`, after lifecycle initialization succeeds |

The [Windows Dockerfile](../../images/runner-windows/Dockerfile) supplies a
`runner` target and a derived `codex` target. See its
[build instructions](../../images/runner-windows/README.md). These images are
built separately from Linux images; build/load them on a Windows-capable runtime.
They use the container-local administrator for development tools and credential
symlinks, and do not request HostProcess access.

Specify a Windows image on each Workspace, or configure the manager's
`--windows-runner-image` / `RC_WINDOWS_RUNNER_IMAGE` default. An absent Windows
default produces `ImageRequired`, rather than selecting the Linux runner.
An OS/node-selector conflict produces `OSPlacementConflict`. OS is immutable,
and an Environment clone must use the same OS as its source.

Upgrade runtime images together with the manager: generated runtime Pods now
use `rc-kube health` for readiness, including Linux Pods. The changed runtime
template can replace existing Workspace Pods, so save disposable checkout
changes before rolling out the manager. The live validation used isolated smoke
Pods; it did not upgrade the cluster's installed manager or CRDs.

## Storage and initialization

Use a storage driver that supports Windows and the selected node. Environment
current/draft promotion and Workspace cloning additionally require PVC cloning.
An existing Linux NFS or local-path StorageClass is not evidence of Windows
compatibility. This change does not install a Windows storage driver.

The earlier container stack failed directory renames on `hostPath` and
`emptyDir` mounts. Windows-native SMB backed by NTFS has since passed directory
symlink and minimal pnpm workspace tests; this does not establish compatibility
with Samba servers or every CSI driver. Validate clone, subpath mounts, links,
and rename behavior on the selected storage backend before relying on it.

Windows `lifecycle.initialize` actions run in the supervisor container before
it starts accepting requests. This preserves files created in its writable
layer; a separate init container cannot populate that layer. Scripts use
PowerShell with terminating-error handling and propagate the last native
command's exit code. Check intermediate native command exit codes explicitly
inside a multi-command PowerShell script. Exact `command` argv bypasses the shell.

Pod replacement and suspension discard the local checkout/cache. Initialize
from a repository or retained home snapshot on each start, and save desired
changes before suspension. rc does not silently synchronize local changes back
to storage. Worktree PVC contents, unlike container-local caches, survive Pod
replacement.

## Repository and Worktree mounts

Windows Workspaces accept the existing Repository and Worktree mount API.
Mount paths remain relative, slash-separated paths (for example `source`),
rendered as `C:\workspace\source`. Repository mounts remain read-only and
writable Worktrees retain the same exclusive write lease as Linux Workspaces.

Repository sync and explicit Worktree bootstrap Jobs still run on Linux. Use
a StorageClass that supports both Linux and Windows mounts and CSI PVC cloning
for the parent and child volumes. Changing the child StorageClass does not
convert an existing ext4 parent into a Windows-compatible filesystem.

Linked Worktree volumes also mount at `C:\mnt\rc\worktrees\<name>` so Git for
Windows can resolve their existing `/mnt/rc/worktrees/...` metadata on drive C.
rc does not rewrite that metadata or reset the checkout on each startup.
Generated root checkouts initialize their branch using PowerShell in the main
runtime container; Worktree readiness follows successful runtime initialization.

Keep rebuildable stores outside mounted source directories and use a distinct
virtual store per Worktree. Persistent node_modules links can become stale if
their container-local targets disappear after runtime replacement; reinstall
and verify dependency resolution before reuse. This support does not make CSI
cloning an instantaneous snapshot or impose per-PVC SMB directory quotas.

## Create a Windows Workspace

Edit the image and StorageClass in the
[Windows sample](../../config/samples/workspaces_v1alpha1_workspace_windows.yaml),
then run these commands from the repository root in a POSIX shell:

```sh
kubectl apply -f config/samples/workspaces_v1alpha1_workspace_windows.yaml
kubectl wait workspace/windows --for=condition=Ready --timeout=180s
rcctl run --workspace windows -- powershell.exe -NoProfile -Command '$PSVersionTable.PSVersion'
```

The sample creates a generic runtime with `C:\workspace` as the default working
directory. Application source, dependencies, development commands, and screenshot
instrumentation belong to the application's repository. For example, an Electron
application installs its own Electron version and supplies its capture tooling.

## CLI and agent images

Creation commands accept `--os windows`, `--node-selector key=value`, and
repeatable `--toleration os=windows:NoSchedule`. Workspace creation from an
Environment inherits its OS and placement when those flags are omitted.
Temporary runs inherit the same defaults. Normal process lifecycle, retained
transcripts, environment projection, and agent credential selection apply.

The Codex image puts its native executable and companion tools on PATH so
`rcctl run --workspace <name> -- codex ...` uses exact argv. `.cmd` and `.bat`
scripts require an explicit shell, such as `cmd.exe /d /s /c ...`, or a
PowerShell lifecycle script. Do not rely on shell-less process creation to
execute a batch shim.

## Troubleshooting

### Git SSH authentication fails

Allow the SSH Private Key Credential on the Workspace and select it on the
Agent Process. Use an SSH remote and check access with `git ls-remote <remote> HEAD`.
Git for Windows' bundled SSH can resolve home and drive-qualified `Include`
paths differently from native SSH. rc supplies a temporary configuration with
the selected SSH fragments through `GIT_SSH_COMMAND` and removes it on exit.
An explicit or inherited `GIT_SSH_COMMAND` or `GIT_SSH` takes precedence; check
these overrides and any custom `Include` paths if authentication still fails.

### A command fails before producing output

Check that the client, controller, and runner use compatible versions, then
inspect the Agent Process termination reason and transcript. Preparation
failures after the transcript opens normally produce exit code 125; missing
or unexecutable commands produce 127 or 126. Correct the configuration and
create a new Agent Process: retrying the same identity returns its retained
failure. Cancellation before launch rolls back preparation instead.

## Validation

The tests cover Windows Job Object descendant cleanup, named-pipe requests,
ConPTY start/resize/stop, credential lifetime, PowerShell Unicode and failure
handling, OS-aware Pod generation, and CRD OS immutability. Native runtime tests
run on Windows; controller tests use envtest and fake Kubernetes clients.
The normal isolated-Kind e2e suite remains separate from Windows-node smoke
experiments. See the [historical rendering experiment](../research/windows-electron-experiment.md).

Application integration fixtures should live in independent repositories and
exercise real Workspace Repository/Worktree mounts: checkout layout, dependency
installation, filesystem operations, source reload, artifacts, and process
cleanup across suspension or Pod replacement. Copying a demo into the container
writable layer does not validate that path.

The earlier Electron experiment established native rendering and process
supervision only. It is not evidence for this newer Repository/Worktree mount
path; validate application-specific behavior separately.
