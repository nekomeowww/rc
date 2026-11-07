---
status: accepted
---

# ADR 0010: Inject rc-kube from the controller

The controller and `rc-kube` share a contract: the controller writes the
`rc-kube` command lines of a runtime Pod, and `rc-kube` must understand them.
Before this decision, the contract had no version. The runner images contained
`rc-kube` (`Dockerfile.runner` at `/usr/local/bin/rc-kube`, `runner-wayland`
at `/usr/local/libexec/rc-kube`). A Workspace keeps the image that it started
with in `status.runtimeImage`. But the controller writes command lines for its
own version.

This broke after an upgrade. A release added the `rc-kube` flag
`--hide-kubernetes-environment`. After the controller upgrade, a Workspace on
an older runner image got a runtime Pod with the new flag. The older `rc-kube`
in that image stopped with `rc-kube: error: unknown flag`, and the Pod restarted
again and again. Each new flag or argument can cause the same failure.

## Decision

The controller gives every Linux runtime Pod an `rc-kube` of its own version.
The image gives only the operating system and the userland.

### The rc-kube image

Each release publishes `ghcr.io/nekomeowww/rc/rc-kube:<version>` for
`linux/amd64` and `linux/arm64`, with the controller and runner images. The
release workflow builds it from `Dockerfile.rc-kube`, and `make docker-build`
builds it with the controller image. The image is `FROM scratch` and has two
static files:

- `/usr/local/bin/rc-kube`, built from the same commit as the controller; and
- `/usr/local/bin/tini`, the static build of the tini release that the runner
  image gets from Debian (0.19.0), pinned by its SHA-256 checksum.

The image has no shell. `rc-kube install DIRECTORY` copies the running
`rc-kube` and the `tini` next to it into the directory. It writes a temporary
file and then renames it, so it is safe to run again. This is simpler and more
robust than a shell copy step: the image needs no shell, no `cp`, and no
second base image.

### Linux runtime Pods

The manager flag `--rc-kube-image` selects the image. The default is the
official image for the version of the controller, which the release build
records with `-ldflags`. A development build uses `latest`. The kustomize
manager config sets the flag like `--runner-image`, because a cluster can use a
registry mirror. The release install manifest sets the image of the same
release.

Every Linux Workspace runtime Pod and WorkspaceEnvironment editor Pod gets:

1. A first init container, `rc-kube-install`, from the rc-kube image. It runs
   `rc-kube install /opt/rc/bin` into an emptyDir.
2. A read-only mount of that emptyDir at `/opt/rc/bin` in the runtime
   container and in every Workspace initializer container.
3. `/opt/rc/bin/rc-kube` as the executable of `serve`, the initializers, the
   `beforeStop` hook, the readiness probe, and every Pod exec of the
   controller and `rcctl`.

When `rc-kube serve` is PID 1, it starts under the `tini` next to it, so the
injected `tini` is used. The socket (`/run/rc/rc-kube.sock`), the state
directory, the home, and the `rcctl` paths do not change. The editor Pod
follows the same rule as the Workspace runtime, because it runs the same
`rc-kube serve` and Pod exec commands, and an Environment image can be old
too. Only the `configure-sudo` step of the editor still uses the shell of the
image.

### Processes find the injected rc-kube on PATH

On Linux, `rc-kube` puts its own directory first on `PATH` when `PATH` does not
have it yet. Every process that `serve` or `lifecycle` starts inherits this
`PATH`: services, executions, terminals, and lifecycle actions. A service that
runs `rc-kube process touch` thus reaches the injected `rc-kube`, also in an
image without `rc-kube`. The directory contains only `rc-kube` and `tini`, so
the change hides no other tool. A process that sets its own `PATH`, or a login
shell that resets it, does not get the directory. Such a process can call
`/opt/rc/bin/rc-kube` directly.

### Images with their own init system

An image can start `rc-kube serve` under its own init system. It sets
`RC_KUBE_INIT` to a program. When `rc-kube serve` is PID 1, it runs
`$RC_KUBE_INIT <rc-kube path> serve <arguments>` instead of `tini`. The
`runner-wayland` image uses this to start s6-overlay and its desktop session,
and then runs the injected `rc-kube` as an s6 service.

Older `runner-wayland` images instead put a script named `rc-kube` on `PATH`.
When `RC_KUBE_INIT` is not set and the first other `rc-kube` on `PATH` is a
script, `rc-kube serve` gives the start to that script, as before. Thus such
an image keeps its desktop session. It still runs the `rc-kube` of its image,
so it gets the version contract only after a move to a newer image. An
`rc-kube` binary in an image is never used for `serve`.

### Upgrade and drift

The topology hash of a Linux runtime gets one new value: where the runtime runs
`rc-kube` from (`/opt/rc/bin`). The hash does not include the rc-kube image.

- After the upgrade to this release, the hash of each Linux runtime Pod
  changes once. The normal topology rule applies: the controller replaces a
  running Pod when it has no active execution, and a Pod with active
  executions keeps running until they finish. A Pod that restarts again and
  again because of an unknown flag is replaced in the same way, and the new Pod
  starts with the injected `rc-kube`.
- Until it is replaced, a Pod from an older controller keeps working. The
  controller finds the `rc-kube` of a Pod from the Pod itself: a runtime
  container that does not run `/opt/rc/bin/rc-kube` keeps the `rc-kube` of its
  image for Pod exec.
- A later controller upgrade changes only the rc-kube image. It does not
  replace a running Pod. The next runtime Pod, for example after a suspend and
  a start, gets the new `rc-kube`. A Pod has one `rc-kube` from start to end,
  so its command lines and its binary always match.
- A suspended Workspace has no Pod. It gets the injected `rc-kube` when it
  starts.
- Editor Pods have no topology hash. An existing editor Pod keeps running, and
  the next editor Pod gets the injected `rc-kube`.

Old runner images still have `rc-kube` on disk. The controller does not use it
for runtime Pods.

### Windows and Darwin

Windows and Darwin runtimes do not change. Their images or VMs still provide
`rc-kube` (`rc-kube.exe` on Windows, `/usr/local/bin/rc-kube` on Darwin), and
their topology hash does not change. A Windows container cannot mount a Linux
binary, and a Darwin runtime is a VM that a provider starts without init
containers.

## Consequences

- A Linux image needs no `rc-kube` and no `tini`. It needs a POSIX environment
  for the processes that it runs, and the user and directories of the runtime
  contract.
- `rcctl` reads the transcript of a suspended Workspace with a short-lived
  helper Pod. `rcctl` does not know the rc-kube image of the controller, so
  this helper still runs `rc-kube transcript` from the Workspace image. An
  image without `rc-kube` cannot show the logs of a suspended Workspace.
- Each Linux runtime start pulls one more small image. Clusters that mirror
  images must mirror the rc-kube image and set `--rc-kube-image`.
- Pod exec from a newer controller into a Pod that started with an older
  controller still crosses versions, as before this decision. This ADR does
  not add a version check for these commands.
- Image upgrades are a separate userland concern. A Workspace keeps its image
  until a user changes it. A later ADR will decide how image upgrades happen.

## Rejected options

- **Environment variables instead of new flags.** An older `rc-kube` ignores an
  unknown variable, so new options would not stop a Pod. But an older binary
  also silently ignores the behavior that the controller asks for, such as
  hiding the Kubernetes variables. Every future option would need this design,
  and other changes, such as new arguments or a new file layout, would still
  break.
- **Protocol version negotiation.** The controller could ask the `rc-kube` of
  an image for its version and write only command lines that it understands.
  The controller would then have to keep the command lines of every older
  version, and it could not know the version before the Pod runs. A runtime
  Pod would also start without the features that the controller promises.
- **Upgrade the image on start.** The controller could move a Workspace to the
  runner image of its own version at each start. This mixes two concerns: the
  supervisor that rc owns, and the userland that the user selected and may have
  changed in place. It also does not help with custom images. Image upgrades
  will get their own ADR.
