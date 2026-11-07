---
status: accepted
---

# ADR 0011: Workspace image upgrades

Before this decision, a Workspace kept its creation image for its full life.
`status.runtimeImage` recorded the image string at creation, and the
controller only read it. A change of `spec.image` had no effect. A blank
Workspace without `spec.image` kept the `--runner-image` of the controller
that created it, also after a controller upgrade.

[ADR 0010](0010-inject-rc-kube-from-the-controller.md) moved `rc-kube` out of
the image on Linux. The image now gives only the operating system and the
userland, and the home volume is persistent. An image upgrade is thus like an
operating system update of a machine: the system files change and the home
stays.

A platform that runs managed devices on rc moves to a newer runner image from
time to time. Its existing Workspaces must move to that image. Then nodes that
pull the current image before use serve all starts, and the platform can delete
old images.

## Decision

The image of a blank Workspace can change. The change applies only when the
controller creates a new runtime Pod. The controller never deletes a running
Pod to apply an image change.

### Image sources

A blank Workspace (no `environmentRef`) has a desired image:

- `spec.image`, when it is set; or
- the base image of the controller for the operating system:
  `--runner-image` on Linux, `--windows-runner-image` on Windows, and
  `--darwin-runner-image` on Darwin.

A blank Workspace without `spec.image` thus follows the base image. After a
controller upgrade that changes `--runner-image`, each such Workspace moves to
the new base image at its next start. The user did not select an image. The
base image is the platform default, and the platform keeps it current. A user
who wants a fixed image sets `spec.image`.

A Workspace cloned from a WorkspaceEnvironment does not change its image. Its
home is a clone of one Environment revision, and that revision was built on
the Environment image. Tools in the home can depend on the system libraries of
that image. A new Environment image or revision only sets `Outdated` with
reason `EnvironmentAdvanced`, as before. `spec.image` stays ignored for an
Environment clone. To move to a new Environment image, create a new Workspace
from the Environment.

### When the change applies

The controller compares the desired image with `status.runtimeImage` at each
pass. When they are different:

- If a runtime Pod exists, the Pod keeps its image. This includes a Pod that
  is starting, Ready, or stopping. The topology hash uses
  `status.runtimeImage`, so an image change does not change the hash and does
  not replace the Pod. The Workspace reports `Outdated=True` with reason
  `ImageChangePending`, and the message names the desired image.
- If the Workspace is `Suspended`, nothing changes. `status.runtimeImage`
  keeps the image of the last runtime Pod, and `Outdated` stays
  `ImageChangePending`.
- If the Workspace is `Running` and no runtime Pod exists, the controller
  writes the desired image to `status.runtimeImage` first. Then it creates the
  Pod on that image in the same pass.

The usual case is the next start after a suspend, for example after an idle
timeout or a maximum run. A runtime Pod that the controller creates for another
reason also uses the desired image: a Pod that is gone after an eviction, a Pod
that the controller replaced after it reached a terminal phase, and a Pod that
the controller replaced after a topology change. In each case, no Pod runs, so
no process is lost because of the image.

The controller writes `status.runtimeImage` before it creates the Pod. If the
status write fails, no Pod exists and the next pass tries again. If the Pod
create fails, the recorded image is still correct for the next Pod. A Pod that
exists thus always has the image of `status.runtimeImage`, and its topology
hash matches.

### Record

- `status.runtimeImage` is the image of the current or last runtime Pod. It
  changes when a runtime Pod starts on a new image.
- The `Outdated` condition of a blank Workspace is `True` with reason
  `ImageChangePending` while the desired image is different, and `False` with
  reason `Current` when the runtime uses the desired image.
- The controller logs `Changed Workspace runtime image` with the previous and
  the new image. The controller does not publish Kubernetes events, so this
  decision does not add one.

### Persistent home state

The home volume does not change. On Linux, the state of `rc-kube` below
`~/.rc` and the transcripts belong to the injected `rc-kube` of the controller
version, not to the image. An image change does not change their format.

Tools and caches that a user installed in the home can depend on the system
libraries of the old image. A platform that changes the base image must keep
the image compatible with the homes that it serves, for example the same
distribution release and the same user, UID 1000, and directory layout.
Changes to the container root filesystem never survive a Pod replacement, so an
image change loses no state that a restart keeps.

### Rollback

A rollback is the same operation in the other direction. Set `spec.image` back
to the earlier image, or set `--runner-image` back for Workspaces without
`spec.image`. The next runtime Pod uses that image. To apply a change at once,
suspend the Workspace and start it again.

### Windows and Darwin

The same rule applies. Windows and Darwin images still contain `rc-kube`, so
on these systems an image change also changes the `rc-kube` that the runtime
runs. A move to a newer image brings an `rc-kube` that matches the controller
better. A rollback to an image from before the controller version can bring
back the version mismatch that ADR 0010 describes, because the controller
writes command lines for its own version.

## Consequences

- A Workspace that never stops never moves to a new image. `maxRunDuration`
  gives a limit when the platform needs one.
- A controller upgrade with a new `--runner-image` does not restart Workspaces.
  It moves them one at a time, as they start.
- `rcctl` reads the transcript of a suspended Workspace with a helper Pod on
  `status.runtimeImage`, the image of the last runtime Pod. Keep an old image
  until the Workspaces that last ran on it have started again, or their
  offline logs cannot be read. The logs of a running Workspace do not need the
  image.
- A Workspace that a release before this decision created moves to its desired
  image at its next start. For a Workspace without `spec.image`, that is the
  current `--runner-image`.

## Rejected options

- **Replace idle running Pods.** The controller could replace a runtime Pod
  without active executions, as for other topology changes. But an idle Pod
  can still run terminals, resident services, and processes that reported
  activity with `rc-kube process touch`. A new `--runner-image` would also
  restart every idle Workspace at the same time after a controller upgrade,
  and all nodes would pull the image at once. The start after a suspend solves
  the need without this risk.
- **Keep the creation image, and change only through recreation.** This was
  the old behavior. It keeps old images in use for the full life of a
  Workspace, so nodes need many images and old images cannot be deleted.
- **Move Environment clones too.** The home of an Environment clone was made
  on the Environment image, so a new image can break it. The Environment is
  the unit that a user edits and commits; moving its clones without their home
  revision would mix two revisions.
- **Pin the base image at creation and follow it only on request.** This
  needs a new API field for a choice that `spec.image` already gives. A
  Workspace without `spec.image` asks for the platform default.
- **Publish an event.** The controller has no event recorder. The condition
  and the status field already show the change to clients that watch the
  Workspace.
