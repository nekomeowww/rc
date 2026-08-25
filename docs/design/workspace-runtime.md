# Workspace runtime v1 design

Status: Accepted design baseline

This document specifies the first Workspace runtime for rc. It records the
resource boundaries, lifecycle rules, CLI behavior, and implementation order
agreed before API scaffolding begins. The example resource shapes are
directional; exact Go field nesting and validation markers remain implementation
work.

## Goals

- Represent a coding environment as a persistent development machine rather
  than one disposable Pod per command.
- Run multiple concurrent coding agents and arbitrary commands in that machine.
- Keep processes alive when an attached terminal disconnects.
- Reconnect multiple terminal clients to one live process.
- Reuse prepared user-level toolchains without reinstalling them for every
  Workspace.
- Combine multiple Worktrees, configurations, and credentials in one
  same-namespace trust boundary.
- Allow an agent running in a Workspace to use `rcctl` to configure other
  Environments and Workspaces and dispatch more Agent Processes.

## Non-goals

The first version will not:

- hot-plug a new PVC into a running Pod;
- store a complete Linux root filesystem in a PVC;
- restart or reconstruct an Agent Process after its runtime Pod is lost;
- isolate credentials from other processes in the same Workspace;
- write directly to a Repository parent volume;
- resolve mutable image tags to digests;
- retain Environment revision history;
- fall back to a copy Job when a StorageClass cannot clone PVCs; or
- create Services or Ingresses for processes running in a Workspace.

## Resource model

The new resources use `workspaces.rc.ayaka.io/v1alpha1`.

```text
WorkspaceEnvironment
  current PVC ─────── clone ──────┐
  draft PVC                       │
                                  ▼
Repository ── clone ── Worktree ── Workspace
                                      │
Credential / AgentCredential ─────────┤
ConfigMap / Secret ───────────────────┤
                                      │
                                      ├── AgentProcess
                                      ├── AgentProcess
                                      └── AgentProcess
```

An Agent Process may instead target the draft of a Workspace Environment. This
allows `env edit` and `env exec` to reuse the same process and terminal runtime.

All resource references are same-namespace. This matches Secret references and
the Kubernetes restriction that PVC clone sources and targets share a
namespace.

The Kubernetes kind is `WorkspaceEnvironment`, with plural
`workspaceenvironments`. Its kubectl short name is `env`; the user-facing CLI
also uses `rcctl env`.

## Runtime programs

rc is split into three programs:

- `rc-controller` runs the Kubernetes controllers and webhooks. The existing
  Kubebuilder manager entry can continue to build this binary without moving
  the scaffolded manager files.
- `rc-kube` runs inside Workspace and Environment editor Pods. It supervises
  processes, owns PTYs and transcripts, and serves local attach requests.
- `rcctl` is the user-facing CLI. It is also present in the Workspace image so
  a running agent can manage rc resources in its namespace.

`rc-kube` exposes a Unix socket inside its Pod. It does not expose a Service or
add an rc-specific network authentication or TLS layer. `rcctl` reaches the
socket by using Kubernetes Pod exec to invoke a thin `rc-kube process attach`
bridge. Kubernetes authorization on Pod exec is the access boundary. A
disconnected bridge does not own or stop the supervised process.

## Runtime images

rc publishes two core images under one product path:

- `ghcr.io/nekomeowww/rc/controller` contains the distroless manager; and
- `ghcr.io/nekomeowww/rc/runner` contains `rc-kube`, `rcctl`, the shell, Git,
  certificates, SSH, the LobeHub CLI (`lh`, `lobe`, and `lobehub`), and the
  system tools required by runtime processes.

The runner image is shared by blank Workspace runtimes, Repository bootstrap
and Exec Jobs, and ordinary Worktree bootstrap Jobs. A generated Worktree that
reuses its cloned Repository root is initialized in the consuming Workspace Pod
with the controller's runner image, avoiding a second Pod and iSCSI attach cycle
for a one-second checkout. User-configured Workspace lifecycle actions still run
in the Workspace runtime image so they see the same toolchain. Workspace
Environment images may extend or replace the runner image, provided they retain
the matching `rc-kube` runtime contract.

Controller and runner images from one release use the same version tag. Release
manifests pass the corresponding runner image to the controller explicitly;
runtime Pods do not follow an unrelated `latest` tag.

## Workspace Environment

A Workspace Environment is a reusable source for a Workspace runner image and
the persistent `/home/agent` toolchain state.

The runner image supplies the base operating system, shell, certificates, and
system libraries. The Environment volume supplies user-level tools and
configuration such as prototools installations, Node.js, Python, Rust, package
caches, and agent configuration. The image value is used exactly as written;
rc does not resolve tags to OCI digests.

### Current and draft state

An Environment maintains at most two volumes:

- `current` is the source cloned by new Workspaces.
- `draft` is a mutable copy used by Environment edit processes.

The lifecycle is:

1. Creating an Environment establishes a blank current state using its image
   and storage configuration.
2. The first `env edit` or `env exec` after a commit clones current into draft.
3. An editor Pod mounts draft at `/home/agent` and runs `rc-kube`.
4. Edit and exec operations create Agent Processes targeting the draft.
5. `env commit` is rejected while a draft Agent Process is active.
6. A successful commit stops the editor Pod, waits for the draft filesystem to
   unmount cleanly, promotes draft to current, increments the content revision,
   and deletes the previous current PVC.

Leaving an `env edit` shell does not discard draft. A later edit or exec resumes
from the same draft until it is committed or the Environment is deleted.

An Environment keeps only its latest committed revision. Existing Workspaces
are independent clones; they continue using the state from which they were
created and report `Outdated` after a later Environment commit. rc does not
update them automatically.

PVC cloning is a required storage primitive, not an optimization. Source and
target StorageClasses must support cloning and use compatible volume modes. If
they do not, Environment draft creation or Workspace creation fails with an
explicit condition and reason. rc never falls back to a copy Job.

Repository, Worktree, Environment, and Workspace Pods use the same UID, GID,
and fsGroup (`1000`) for persistent storage. They set
`fsGroupChangePolicy: OnRootMismatch`, so a clone whose root ownership already
matches does not trigger a recursive ownership walk. This is required for
small-file-heavy repositories on network filesystems, where an unnecessary
recursive chmod/chown can dominate startup time.

The editor Pod remains available for ten minutes after its final process exits
so a coding agent can issue consecutive `env exec` commands without repeated
cold starts. The timeout is configurable, and zero disables automatic editor
suspension. Suspending the editor never deletes draft.

The default Workspace image runs as the non-root `agent` user and includes the
`sudo` executable, but it does not contain a passwordless sudoers rule. Normal
Workspace runtime Pods also disable privilege escalation and drop all Linux
capabilities, so Agent Processes cannot use `sudo` or become root.
The runtime security-policy version participates in the Workspace topology
hash, so a policy change rolls an idle runtime Pod through the existing guarded
topology-replacement path.

Environment editor Pods are the explicit exception. They inject a temporary,
root-owned sudoers rule for `agent` and permit the supervisor container to use
the `sudo` setuid helper. This gives `env edit` and `env exec` passwordless root
without making the Pod privileged. The rule exists only in an editor-scoped
ephemeral volume and is never stored in the image or Environment PVC.

Changes to the container root filesystem do not survive editor Pod replacement
and are not part of an Environment commit. Durable system packages therefore
belong in the runner image; tools installed below `/home/agent` persist in the
Environment volume.

### Environment CLI

The initial command surface is:

```text
rcctl env create <name>
rcctl env edit <name>
rcctl env exec <name> [--] <command> [args...]
rcctl env commit <name>
rcctl env stop <name>
rcctl env delete <name>
```

`env edit` creates a terminal Agent Process running a shell. `env exec` creates
a non-terminal Agent Process, waits for it to finish, streams stdout and stderr,
and returns the command's exit code. Both survive client disconnect through the
same rules as Workspace processes and receive an rc process ID. `env stop`
stops an idle editor without deleting draft and is rejected while a draft
process is active. Environment edit and exec operations do not select a
Credential unless the caller explicitly requests one.

## Workspace

A Workspace is a persistent development machine. It owns one mutable home
volume, one runtime Pod while running, persistent process transcripts, and
references to its code, configuration, and credentials.

### Environment and filesystem

`environmentRef` is optional:

- with an Environment, the Workspace home volume clones the Environment current
  PVC, records the source content revision, and snapshots the Environment's
  runner image string. Runtime replacements continue using that captured image;
  a later image or revision change only makes the Workspace `Outdated`;
- without one, the Workspace receives a blank home volume and uses the rc base
  Workspace image. The base contains `rc-kube`, `rcctl`, a shell, Git, CA
  certificates, Node.js with Corepack, common build and archive tools, and the
  proto and mise tool-version managers. It does not promise Python, Rust, or
  another language toolchain.

The stable filesystem layout is:

```text
/home/agent                         persistent home, tools, caches, agent state
/workspace/<mount-name>             Repository or Worktree mounts
/run/rc/credentials/...             temporary credential material
/run/rc/processes/<process-id>/...  per-process temporary configuration
```

Mount names and paths are unique. By default, a `--repo foo` shortcut names its
mount `foo`; a collision requires an explicit mount name. User-selected code
paths remain beneath `/workspace`; they cannot replace the home or rc runtime
paths. A Workspace may set a default working directory, and an Agent Process
may override it with `--cwd`. When neither is set, the process starts in
`/workspace` rather than guessing a primary repository.

### Repository and Worktree mounts

A Repository parent is synchronized, authoritative source state. It is never a
normal writable code mount:

- an existing Worktree is mounted read-write;
- a Repository may be mounted only through an explicit read-only operation; and
- the `--repo <name>` shorthand, when constructing or explicitly mounting
  topology, creates a new Worktree and mounts it read-write.

Repository and Worktree selectors are repeatable. Selecting a Repository and
an existing Worktree belonging to it creates two independent mounts; neither
silently replaces the other.

An automatically created Worktree starts from the Repository synchronized ref
on a unique `rc/<workspace>/<mount>` branch. It remains an ordinary Worktree
resource and is not deleted automatically when its creating process finishes.
Because its PVC is already an isolated CSI clone with a complete working tree,
the generated Worktree reuses the clone root and creates only the unique branch;
it does not create a second nested checkout. Explicitly created Worktrees retain
native `git worktree add` behavior and its advanced flags.

One Worktree may be mounted read-write by only one running Workspace at a time.
Processes inside that Workspace may use it concurrently. Multiple Workspaces
may mount the same source read-only. The controller releases the write claim
when a Workspace suspends.

### Configuration

ConfigMap, Secret, environment variable, and projected file behavior follows
Kubernetes semantics:

- a Workspace directly references ConfigMaps and Secrets;
- updates to an already projected file are eventually reflected by kubelet;
- adding or removing a projected source changes Pod topology and requires a
  runtime replacement;
- environment values are resolved when an Agent Process starts; and
- a running process never receives environment mutations.

Credentials use Credential and AgentCredential references rather than being
copied into the home volume.

### Compute and scheduling

CPU, memory, accelerators, and scheduling belong to the Workspace runtime Pod.
All processes in a Workspace share its cgroup budget. Per-process resource
isolation is outside v1; a task requiring an independent quota uses another
Workspace.

The Workspace API can carry normal Pod resource requirements and advanced
scheduling settings such as node selection, affinity, tolerations, and
RuntimeClass. The CLI should expose common CPU, memory, and accelerator flags
and leave uncommon scheduling configuration to manifests.

`workspace create`, `agent run`, and `agent exec` expose GPU requests. `--gpu`
sets the `nvidia.com/gpu` count and `--gpu-vram` independently sets the
`nvidia.com/gpumem` quantity. `--gpu-resource NAME=QUANTITY` is repeatable and
adds a vendor-specific Kubernetes extended resource directly. rcctl writes
each selected GPU resource to both runtime container requests and limits. When
an Agent Process targets an existing Workspace, these flags are requirements:
the command rejects a Workspace that does not already provide the requested
quantities instead of mutating its runtime topology.

### Start, suspend, and topology changes

A Workspace has a desired runtime state:

- `Running` ensures one runtime Pod exists.
- `Suspended` removes the runtime Pod but retains the home volume, Worktrees,
  configuration, Agent homes, process records, and transcripts.

Named Workspaces default to `Running` when created.

`rcctl workspace start` and `rcctl workspace stop` change this state. A normal
stop is rejected while a process is active. A forced stop terminates the
processes and then suspends the Workspace.

Kubernetes does not permit adding PVC volumes to an existing Pod. Mount,
Environment, Service Account, and projected-volume topology changes therefore
replace the runtime Pod. A change is rejected while Agent Processes are active
unless the caller explicitly forces termination. rc will not use privileged
mount propagation as a default hot-plug mechanism.

Named Workspaces remain running unless stopped or configured with an idle
timeout. Automatically created Workspaces suspend after their processes finish,
but their resources and volumes remain until explicit cleanup.

### Runtime lifecycle actions

A Workspace may define ordered `spec.lifecycle.initialize` and
`spec.lifecycle.beforeStop` actions. Each action contains exactly one of:

- `command`, an exact argv executed without shell interpretation; or
- `script`, source executed by `/bin/sh -ceu`.

An optional `workingDirectory` defaults to `/workspace`. Initialize actions run
as init containers, in order, after all Workspace volumes are mounted and before
the `rc-kube` supervisor starts. They run again for every replacement runtime
Pod and must therefore be idempotent. A failed initialize action prevents the
Workspace from becoming Ready.

Before-stop actions use the runtime container's Kubernetes pre-stop hook. They
run in order during a normal suspension, topology replacement, or deletion, but
remain best effort: Kubernetes continues termination after a failure or when
the Pod termination grace period expires. They do not run after the container
has already exited or during an ungraceful node loss.

Generated Worktrees use the same lifecycle action runner internally. Their PVC
becomes `VolumeReady` as soon as the clone is bound; the Workspace then performs
an idempotent branch checkout in its first init container. The Worktree
controller observes that init container and owns the final Worktree `Ready`
condition. Ordinary independently managed Worktrees retain their bootstrap Job.

## Agent Process

AgentProcess is the only process CRD. `agent run` and `agent exec` are different
I/O defaults for the same resource, not separate API kinds.

### Desired state

An Agent Process records:

- a target Workspace or Workspace Environment draft;
- the exact argv supplied by the caller;
- an optional working directory;
- whether to allocate a PTY;
- process-specific environment and configuration inputs; and
- a one-way desired state from running to stopped.

The Kubernetes object name is the rc process ID. rcctl generates a sortable,
agent-prefixed name such as `codex-01k2...` for a recognized adapter and a
generic `process-...` name otherwise. Commands such as `resume`, `logs`, `stop`,
and `delete` accept a namespace-unique prefix for interactive convenience;
scripts and Kubernetes API clients use the full name. The Agent Process spec is
otherwise immutable after creation. Agent-native session IDs are separate and
never substitute for this ID.

The status lifecycle is:

```text
Pending -> Starting -> Running -> Succeeded
                              \-> Failed
                              \-> Stopped
                 runtime loss -> Lost
```

An exit code of zero produces `Succeeded`; another exit code produces `Failed`.
`Stopped` records an explicit stop request. `Lost` means the original process
no longer exists because its runtime disappeared. Terminal states never return
to Running. Terminal status retains the exit code when available, completion
time, termination reason, and the persistent transcript index.

### Start sequence and at-most-once execution

Process creation is declarative:

1. `rcctl` creates the AgentProcess object and immediately prints its ID.
2. `rc-controller` resolves a ready target runtime.
3. The controller invokes `rc-kube process start <id>` through Pod exec.
4. `rc-kube` uses the AgentProcess UID as its idempotency key and starts the
   command at most once.
5. The controller records Running after the supervisor acknowledges ownership.
6. `rcctl` attaches after Running is observed.

The client never starts an unmanaged process and then backfills a CR. A
controller retry with the same UID returns the existing process. A runtime Pod
restart does not repeat a command whose effects may already have occurred.

### Commands and agent adapters

The command argv is executed exactly as supplied. Adapters do not insert,
remove, reorder, or reinterpret command arguments.

An adapter for a recognized coding agent may supply default environment,
credential paths, status detection, and Agent home layout. An unrecognized
command still runs, but rcctl warns and does not automatically select an
AgentCredential. A caller explicitly selects generic Credentials with repeated
`--credential <name>` flags. There is no flag that implicitly exposes every
Credential. Credentials use `/run/rc/credentials/<name>/`, and
`RC_CREDENTIALS_DIR` points to `/run/rc/credentials`. A Process Credential may
independently configure raw Secret-backed `files` and non-secret literal
`envs`; selecting it applies both sets of projections.

### Terminal and attach behavior

`agent run` defaults to a PTY and `agent exec` defaults to non-terminal I/O. A
PTY process supports bracketed input, resize, signals, and an unmodified
terminal byte stream; client disconnect leaves the PTY and process owned by
`rc-kube`.

Multiple clients may attach read-write to one process. The supervisor
serializes their input. The client that most recently produces keyboard,
mouse, paste, or focus interaction becomes foreground and determines the
shared PTY size. `rc-kube` appends bytes read from the PTY master to the bounded
transcript and broadcasts those exact same bytes to attached clients without
parsing or rendering them. Attach first replays that raw transcript and then
follows live output without a gap. The attach request carries the client's
initial size so registration and the first PTY resize happen atomically before
its input is forwarded. Each attach has a stable client ID and remembered
viewport. Because one PTY has one size, background clients do not receive an
independently rendered viewport. Slow clients are explicitly disconnected
instead of silently losing output.

`rcctl agent resume <id>` means reconnect to that rc-managed live process. It
does not refer to a Codex or other agent-native session ID. If the process or
runtime no longer exists, resume fails; it neither creates another process nor
invokes an agent-native resume command.

### Stop and logs

`rcctl agent stop <id>` changes the one-way desired state. The supervisor sends
SIGTERM to the process group, waits for the configured grace period, and then
uses SIGKILL. Stopped processes cannot be started again. Deleting the CR is a
separate operation.

Terminal output is appended to the target persistent volume and is not stored
in CR status. It may contain source code, command output, and secrets printed by
the process, so the Workspace volume is sensitive data. Log storage has a
configurable bound and reports truncation. The append-only raw transcript is
also used to seed a newly attached terminal and survives process exit and
runtime restarts. When the target runtime exists, `agent logs` reads through
`rc-kube`. Otherwise the controller creates a short-lived read-only helper Pod
that mounts only the target log volume. The helper does not mount Worktrees or
Credentials and does not change the target desired state.

## Credentials and Agent homes

A Workspace is one trust boundary. Processes running as the same user can
inspect each other's files and process environment, so process-level Credential
selection is a convenience and compatibility feature rather than strong
isolation. Tasks that do not trust each other use different Workspaces.

Workspace AgentCredential references are ordered:

- one compatible AgentCredential is selected automatically;
- with several compatible references, the first is the default; and
- a process may select another reference explicitly.

The selected default is exposed at the adapter-native path. Alternate
credentials use their own Agent homes; for Codex, the adapter points each
process at the corresponding separate `CODEX_HOME`.

Agent home state is scoped by Workspace, Agent Type, and Agent Credential. All
matching processes share configuration, caches, and agent-native session
metadata, for example under
`/home/agent/.rc/agents/codex/<credential-name>/`. Secret files such as Codex
authentication are projected through a temporary runtime path and linked or
otherwise exposed inside the Agent home; they are never copied into the
persistent home PVC. A restarted runtime receives a fresh Secret projection
while retaining the non-secret Agent home.

Environment edit operations do not permanently associate Credentials with an
Environment. A process targeting an Environment draft may request a Credential
for that operation, but commit does not copy the credential projection. A
command that deliberately copies a token into `/home/agent` is user-controlled
behavior; rc does not scan committed files for secrets.

## Environment pass-through

Commands inherit the environment of the calling `rcctl` process by default.
`--no-env-passthrough` disables this behavior. `--env NAME`, `--env NAME=value`,
and `--env-file` provide selective input and explicit overrides.

Automatic pass-through excludes values that describe the caller's local paths,
terminal, Kubernetes client, shell integration, IDE session, or agent runtime.
This includes well-known names and families such as:

```text
PATH
HOME
PWD
OLDPWD
USER
LOGNAME
SHELL
SHLVL
_
KUBECONFIG
KUBERNETES_*
XDG_RUNTIME_DIR
XDG_*
SSH_AUTH_SOCK
TMP
TEMP
TMPDIR
TERM
COLORTERM
TERM_*
LINES
COLUMNS
ATUIN_*
CODEX_*
GEMINI_CLI_IDE_*
GHOSTTY_*
HOMEBREW_*
MISE_*
NIX_*
STARSHIP_*
SWIFTLY_*
VSCODE_*
VOLTA_*
XPC_*
__*
RC_*
```

Names that conventionally point at host filesystem locations are also
excluded, including names ending in `_PATH`, `_DIR`, `_HOME`, `_ROOT`,
`_PREFIX`, `_FILE`, or `_SOCK`, plus conventional path variables such as
`FPATH`, `GOPATH`, `MANPATH`, `PYTHONPATH`, and `PYTHONSTARTUP`. Host runtime
hooks such as `GIT_ASKPASS` and `NODE_OPTIONS` are excluded for the same
reason.

These names are not protected. An explicit `--env` may add any of them back.
The effective precedence is target runtime defaults, Workspace configuration,
automatic caller pass-through, env files, and explicit env flags. Agent
adapters only fill values that remain unset. rcctl warns if an override is
likely to break a recognized adapter, but it does not reject the value. rc
process identity and supervisor control never depend on environment variables
that a caller can override.

Passed values are stored in a temporary Secret owned by the AgentProcess rather
than inline in the CR. The controller reads that Secret when starting the
process. Its OwnerReference removes it with the AgentProcess record.

## Kubernetes access from a Workspace

A Workspace is Kubernetes-enabled by default but is not a Linux privileged Pod.
The controller ensures one shared `rc-workspace` Service Account and supporting
Role/RoleBinding in each namespace that needs it.

The default Role can manage, in its namespace:

- all rc custom resources;
- Secrets and ConfigMaps required by Credential import and Workspace
  configuration; and
- Pod exec, logs, and port-forward needed to start, attach to, inspect, and
  reach child runtimes.

It does not grant general management of Deployments, StatefulSets,
RoleBindings, or other unrelated Kubernetes resources.

Workspace creation supports three direct choices:

```text
default                         use the namespace rc-workspace Service Account
--no-service-account            disable Service Account token mounting
--service-account <name>        use that same-namespace Service Account
```

Selecting a Service Account does not add a custom grant layer or an rc-specific
impersonation check. Cluster administrators are responsible for the permissions
available to callers and selected accounts. A more privileged account allows
the Workspace agent to use ordinary Kubernetes and rcctl operations beyond the
default rc Role.

Inside a Pod, rcctl discovers its namespace from the mounted Service Account
namespace file. Outside the cluster, namespace selection follows kubectl:
explicit `--namespace` or `-n`, then the kubeconfig context namespace, then
`default`.

## CLI target resolution

An XDG configuration file may select a default Workspace and a default
Workspace Environment per Kubernetes context and namespace. The default
Environment is used only when constructing a generated Workspace; an existing
Workspace already fixes its own Environment. CLI resource selection uses
`--environment <name>` so `--env NAME[=value]` remains unambiguously reserved
for process environment variables.

The resolution rules for `agent run` and `agent exec` are:

1. An explicit `--workspace <name>` must name an existing Ready Workspace.
2. Without that flag, an existing configured default Workspace is used only
   when at least one `--repo` or `--worktree` requirement is supplied.
3. Environment, Repository, and Worktree arguments supplied with an existing
   Workspace are requirements, not mutations: `--environment` must match and
   each code resource must already be represented by a matching mount. A
   mismatch stops the run; `--repo` does not create a Worktree in this case.
4. With no explicit Workspace and no Repository or Worktree requirement, rcctl
   creates a generated Workspace with no code mounts, even when a default
   Workspace exists.
5. `--temporary` explicitly requests a generated Workspace even when a default
   exists.
6. An explicitly named missing Workspace is always an error and is never
   created as a typo recovery behavior.

A generated Workspace is independent. It does not copy a default Workspace's
live home volume, Worktree mounts, Credentials, ConfigMaps, Secrets, or resource
settings. It uses only command arguments and caller environment pass-through.
If exactly one compatible AgentCredential exists in the namespace, a known
agent may select it automatically; several candidates require explicit ordered
`--agent-credential` flags.

All referenced Environments, Repositories, and existing Worktrees must already
exist and be Ready. While constructing a generated Workspace, `--repo <name>`
is the only shorthand that creates another domain resource: it creates and
mounts a Worktree from that existing Repository. There is no
`--auto-create-worktree` flag.

## CLI surface

The initial Workspace and process commands are expected to include:

```text
rcctl repo clone <url> [--with-submodules] [--recursive-submodules]
rcctl repo delete <name>

rcctl workspace create <name>
rcctl workspace mount repo <repository> [mount options]
rcctl workspace mount worktree <repository>/<worktree> [mount options]
rcctl workspace unmount <workspace> <mount> [--force]
rcctl workspace start <name>
rcctl workspace stop <name> [--force]
rcctl workspace delete <name> [--force] [--cascade-created-worktrees]
rcctl workspace port-forward <name> <local-port>[:<remote-port>]

rcctl agent run [target options] [--] <command> [args...]
rcctl agent exec [target options] [--] <command> [args...]
rcctl agent resume <id>
rcctl agent list [--workspace <name>] [--all-namespaces]
rcctl agent logs <id>
rcctl agent stop <id>
rcctl agent delete <id>
```

Repository clone does not initialize submodules by default.
`--with-submodules` initializes direct submodules;
`--recursive-submodules` initializes nested submodules and implies
`--with-submodules`. These map to an optional `spec.submodules` object whose
`recursive` field controls traversal depth.
`repo delete` (also available as `repo remove` and `repo rm`) deletes the
Repository resource; Kubernetes garbage collection deletes its owned parent
PVC and bootstrap Jobs.

`workspace mount repo` creates and mounts a writable Worktree by default. Its
explicit `--read-only` form mounts the Repository parent itself. Topology
changes follow the active-process rejection and `--force` rules above. Mount
options include `--workspace`, `--path`, `--name`, and `--force`.

For `agent run` and `agent exec`, the first positional argument is the command.
For `env exec`, the Environment name precedes the command. In all three forms,
rcctl options must precede the command. `--` is optional and exists only to
disambiguate a command or argument that would otherwise parse as an rcctl
option. The stored argv is exactly the command and remaining arguments;
adapters do not rewrite it or interpret a task description. Target options
include repeatable `--repo`, `--worktree`, and `--credential` selectors,
`--environment`, and `--cwd`.

`agent exec` uses non-terminal I/O, waits synchronously, streams stdout and
stderr, and exits with the command's exit code. `agent run` defaults to a full
interactive terminal. Both create a persistent AgentProcess before attaching
and can run concurrently with other processes in one Workspace.

`agent list` defaults to every AgentProcess in the current namespace, not only
the XDG default Workspace. Its table includes the process ID, target, command,
TTY mode, phase, attached client count, age, and exit code. It can filter by
Workspace, phase, agent type, and ID prefix, in addition to listing all
namespaces when authorized.

`workspace port-forward` uses Kubernetes Pod port-forward and does not require
a declared container port. Closing the client stops only the forwarding
connection. Long-lived exposure remains an explicit Service or Ingress managed
outside this feature.

## Retention and deletion

Finishing a process does not delete its Workspace, home volume, AgentProcess
record, transcript, or automatically created Worktree. A generated Workspace
is suspended to release compute and retained for inspection. There is no
default TTL that deletes these resources.

Deleting a Workspace:

- terminates its active Agent Processes;
- deletes its runtime Pod, home volume, process records, and transcripts; and
- never implicitly deletes a referenced or automatically created Worktree.

The CLI rejects deletion with active processes unless `--force` is present.
Direct Kubernetes deletion is treated as an explicit forced deletion and uses
a finalizer to ask the supervisor to terminate processes before storage
cleanup.

`rcctl workspace delete --cascade-created-worktrees` previews and deletes
Worktrees labelled as created for that Workspace. A regular Workspace deletion
never performs this cascade, and the cascade never includes Worktrees that
predated and were merely referenced by the Workspace.

The shared namespace `rc-workspace` Service Account and RoleBinding outlive
individual Workspaces.

## Proposed API shapes

These examples communicate ownership and intent. They are not generated CRDs
and do not settle exact field names.

```yaml
apiVersion: workspaces.rc.ayaka.io/v1alpha1
kind: WorkspaceEnvironment
metadata:
  name: rust-node
spec:
  image: ghcr.io/example/rc/runner:latest
  storage:
    storageClassName: clone-capable
    size: 20Gi
  editorIdleTimeout: 10m
status:
  currentRevision: 3
  currentVolumeClaimName: rust-node-current-3
  draftVolumeClaimName: rust-node-draft
  editorPodName: rust-node-editor
  conditions: []
```

```yaml
apiVersion: workspaces.rc.ayaka.io/v1alpha1
kind: Workspace
metadata:
  name: rc-dev
spec:
  desiredState: Running
  environmentRef:
    name: rust-node
  mounts:
    - name: rc
      path: rc
      worktreeRef:
        name: rc-main
    - name: docs-source
      path: docs-source
      repositoryRef:
        name: docs
      readOnly: true
  agentCredentialRefs:
    - name: codex-personal
    - name: codex-team
  credentialRefs:
    - name: github
  serviceAccountName: rc-workspace
  automountServiceAccountToken: true
  resources: {}
status:
  sourceEnvironmentRevision: 3
  runtimeImage: ghcr.io/example/rc/runner:latest
  homeVolumeClaimName: rc-dev
  runtimePodName: rc-dev
  conditions: []
```

```yaml
apiVersion: workspaces.rc.ayaka.io/v1alpha1
kind: AgentProcess
metadata:
  name: codex-01k2example
spec:
  targetRef:
    kind: Workspace
    name: rc-dev
  command:
    - codex
    - implement the task
  workingDirectory: /workspace/rc
  tty: true
  desiredState: Running
status:
  phase: Running
  runtimePodUID: example
  startedAt: "2026-08-20T10:00:00Z"
  attachedClients: 2
  conditions: []
```

An Environment draft target uses `kind: WorkspaceEnvironment` plus an explicit
draft selector in the final API. The scaffold implementation should choose a
union shape that can be validated with CEL without treating the draft as a
separate Kubernetes Kind.

## Implementation stages

### 1. Scaffold the APIs

Use Kubebuilder rather than manually creating API or controller files:

```sh
kubebuilder create api --group workspaces --version v1alpha1 --kind WorkspaceEnvironment
kubebuilder create api --group workspaces --version v1alpha1 --kind Workspace
kubebuilder create api --group workspaces --version v1alpha1 --kind AgentProcess
```

Define reference, storage, mount, target, phase, and condition types. Add CEL
validation for immutable Agent Process execution fields, mount union fields,
same-object target variants, and one-way stop behavior where schema validation
can express it. Mark WorkspaceEnvironment with plural `workspaceenvironments`
and short name `env`. Regenerate manifests and DeepCopy methods after type
changes.

### 2. Build Environment storage and editing

Implement current/draft PVC reconciliation, revision status, editor Pod
creation, idle suspension, and commit coordination. Start with non-interactive
draft commands before the complete terminal client. Verify that the configured
StorageClass supports PVC clone behavior in envtest where possible and in an
isolated Kind end-to-end cluster with a clone-capable CSI driver.

### 3. Build Workspace storage and lifecycle

Implement optional Environment cloning, blank home volumes, runtime Pod
creation, Running/Suspended state, code mount validation, Worktree write
exclusivity, and topology replacement rules. Add the namespace Service Account,
Role, and RoleBinding reconciler. Keep credential material out of the home
volume from the first implementation.

### 4. Build the rc-kube supervisor

Add an `rc-kube` entry point and a versioned local protocol. Implement
idempotent start by AgentProcess UID, process groups, PTY and non-PTY I/O,
signals, exit observation, bounded transcript persistence, and Unix-socket
attach. The base Workspace image must contain compatible `rc-kube` and `rcctl`
binaries.

### 5. Reconcile Agent Processes

Implement controller-to-supervisor Pod exec, status transitions, at-most-once
retries, stop escalation, runtime-loss detection, and Environment draft
targets. A controller restart must find the existing UID-owned supervisor
process rather than create a second process.

### 6. Add rcctl workflows

Add Environment, Workspace, and Agent commands; XDG default Workspace
and Environment selection; namespace resolution; generated Workspace creation;
exact target validation; ordered Credential selection; caller env pass-through;
and temporary Secret cleanup. Treat the first positional argument as the
command, with optional `--` disambiguation, and preserve the remaining argv
exactly.

### 7. Complete terminal and log access

Add multiple attached clients, foreground resize arbitration, raw PTY stream
broadcast, reconnect, list metadata, transcript reads, and the suspended-target
helper Pod. Add Workspace port-forward after runtime Pod discovery is stable.

### 8. Add cleanup and end-to-end coverage

Implement finalizers, forced stop, generated-resource labels, explicit cascade
cleanup, and retention tests. End-to-end tests must use a dedicated Kind
cluster and cover PVC clone requirements, disconnect and reattach, concurrent
processes, runtime loss, Worktree write exclusion, nested rcctl dispatch, and
Service Account opt-out.

After Go changes, run `make lint-fix` and `make test`. After API or marker
changes, run `make manifests` and `make generate` as required by the repository
guide.

## Deferred implementation choices

The accepted design does not yet select:

- exact CRD field nesting, status reason strings, and print columns;
- image repository names and release coupling between rc-controller and
  rc-kube;
- the default transcript byte limit and process stop grace period; or
- controller polling and runtime health-check intervals.

These choices can be made while implementing their owning stage without
changing the resource and lifecycle contract above.

The implementation uses a versioned, line-delimited JSON handshake for local
`rc-kube` control requests and a raw byte stream after attach acknowledgement.
Durable transcripts remain bounded append-only byte logs rather than terminal
events; rc does not emulate or re-render the child terminal.
