# Using `rcctl`

`rcctl` is the day-to-day CLI for managing `rc` credentials, Git repositories, Worktrees, Workspace Environments, Workspaces, and supervised processes.

## Command groups

| Command | Purpose |
| --- | --- |
| `rcctl credentials` | Import Git, agent, and process credentials |
| `rcctl repo` | Clone, sync, inspect, execute commands in, and delete `Repository` mirrors |
| `rcctl worktree` | Create, inspect, execute in, and delete independent Git checkouts |
| `rcctl env` | Prepare, edit, and commit reusable `WorkspaceEnvironment` templates |
| `rcctl workspace` | Create persistent development Workspaces, start/stop them, and manage mounts |
| `rcctl run` / `rcctl exec` | Run commands in a new (`run`) or existing (`exec`) Workspace |
| `rcctl ps` / `attach` / `logs` / `stop` / `inspect` / `rm` | List and manage supervised processes |
| `rcctl cp` | Copy files and directories to and from a running Workspace |
| `rcctl doctor` | Read-only audit of storage, runtime failures, references, and deletion blockers |

List commands render compact, width-aware tables. Pass `-o wide` for secondary columns, or `-o json` / `-o yaml` for complete resources. `inspect`, `workspace get`, `repo get`, and `worktree get` show untruncated detail views.

## Import credentials

Public repositories require no Git credential. All credentials are stored as namespaced `rc` resources backed by Kubernetes Secrets.

### GitHub credential (for Repository clone/sync)

```sh
gh auth login
rcctl -n development credentials import --type github
```

This creates a `Credential` named `github-com`. Pass `--credential-ref github-com` when cloning a private repository:

```sh
# rcctl -n development repo clone https://github.com/OWNER/REPOSITORY.git \
#   --name repository \
#   --storage-class csi-hostpath-sc \
#   --credential-ref github-com
```

### Agent credential (for coding agents)

```sh
rcctl -n development credentials import --type agent --agent codex --file "$HOME/.codex/auth.json"
```

This creates an `AgentCredential` named `codex` in the target namespace.

### Generic process credential

Project an arbitrary credential file into explicitly selected executions:

```sh
# rcctl -n development credentials import \
#   --type process \
#   --name tool-auth \
#   --file ./credentials.json \
#   --mount-path /home/agent/.tool/credentials.json \
#   --env TOOL_HOME=/home/agent/.tool
```

## Repositories and Worktrees

A `Repository` is an authoritative mirror of a Git remote. A `Worktree` is an independent CSI volume clone of that `Repository` PVC with the requested branch initialized directly in the clone root.

### Clone and sync a Repository

```sh
rcctl -n development repo clone https://github.com/nekomeowww/rc.git \
  --name rc \
  --storage-class csi-hostpath-sc

rcctl -n development repo list
rcctl -n development repo exec rc -- git status --short
```

Before creating a new Worktree from recent upstream commits, sync the parent mirror:

```sh
rcctl -n development repo sync rc
```

- `sync` uses the Repository's credential and submodule policy, removes untracked files from the parent, and prints the resolved commit.
- Existing Worktrees keep their commits and local changes.
- Submodule update failures fail the sync by default; set `spec.submodules.failurePolicy: Continue` (or clone with `--continue-on-submodule-failure`) to keep the parent ready and report the failing path on `SubmodulesReady`.
- Completed `RepositorySync` requests expire after 3 days by default (`spec.ttlSecondsAfterFinished`). Pass `--wait=false` to return immediately.
- Sync waits for other parent writers, pending clones, and direct Repository mounts. See [Repository sync](../design/repository-sync.md).

### Create and use a Worktree

```sh
rcctl -n development worktree add --repo rc --name rc-docs --branch docs/readme
rcctl -n development worktree list
rcctl -n development worktree exec rc-docs -- git status --short
```

- `worktree add` supports `--ref`, `--detach`, `--orphan`, `--no-checkout`, `--lock`, and `--reason`.
- `worktree exec` runs short commands in a standalone Job using the base runner image without allocating a Workspace home PVC. Use `rcctl run --rm --worktree <name> -- <cmd>` when you need Environment state, caches, credentials, persistence, or an interactive terminal.
- New Worktrees inherit their source PVC's `StorageClass` and access modes, and default to covering both the source's requested and reported capacity. See [Worktree storage planning](../design/worktree-storage.md).
- Delete an unmounted Worktree and its owned PVC with `rcctl -n development worktree rm rc-docs`.

## Workspaces and Environments

### Temporary isolated run (`rcctl run`)

`rcctl run` always creates a new Workspace:

```sh
rcctl -n development run -it --rm \
  --repo rc \
  --image ghcr.io/nekomeowww/rc/runner:latest \
  --storage-class csi-hostpath-sc \
  --agent-credential codex \
  --cwd /workspace/rc -- codex
```

### Persistent named Workspace (`rcctl workspace` + `rcctl exec`)

```sh
rcctl -n development workspace create dev \
  --image ghcr.io/nekomeowww/rc/runner:latest \
  --storage-class csi-hostpath-sc \
  --agent-credential codex

rcctl -n development workspace mount worktree rc-docs --workspace dev --path rc
rcctl -n development workspace default dev

rcctl -n development exec -it --agent-credential codex --cwd /workspace/rc dev -- codex
```

### Hot-mounting Worktrees on Linux

On Linux, ready Worktree PVCs can be mounted into a running Workspace without restarting its Pod:

```sh
rcctl -n development env create prepared \
  --image ghcr.io/nekomeowww/rc/runner:latest \
  --storage-class csi-hostpath-sc

rcctl -n development workspace create dev-hot --environment prepared
rcctl -n development workspace mount worktree rc-docs --workspace dev-hot --path rc
```

The Workspace keeps its home directory, caches, and running processes when a Worktree is added. Stop active processes before unmounting a Worktree. Windows Workspaces replace the runtime Pod for mount changes.

## Lifetimes and auto-suspension

| Creation path | Default retention | Notes |
| --- | --- | --- |
| `rcctl run` (unnamed) | Temporary (`DeleteAfterProcessesExit`) | Deleted 5 minutes after all processes exit; pass `--retain` (or `--rm=false`) to keep |
| `rcctl run --name <name>` | Retained | Pass `--rm` to make temporary |
| `rcctl workspace create` | Retained | Opt into automatic cleanup with `--idle-timeout` / `--delete-after-suspended` |

- Worktrees generated via `run --repo` are owned by the new Workspace and deleted with it. Existing Worktrees selected with `--worktree` are never deleted by Workspace cleanup.
- Long-lived Workspaces can set `--idle-timeout=1h --delete-after-suspended=168h` (suspend compute when idle, then delete owned storage after a suspended grace period) and `--max-run-duration=8h` (hard cap per start, even while busy).
- Resident services inside a Workspace can report activity with `rc-kube process touch` so idle suspension waits for them.
- See [ADR 0007: Workspace lifetimes](../adr/0007-workspace-lifetimes.md) for the complete behavior matrix.

## Processes, I/O, and file transfer

Put `rcctl` flags before the Workspace name in `exec`; everything after the Workspace name is the command argv.

- Both `run` and `exec` wait for completion and return the command's exit code by default.
- Use `-it` for an interactive terminal, `-d` to detach, or `-i` to pipe stdin (e.g. `tar c dir | rcctl exec -i dev -- tar x`).
- Foreground output is streamed losslessly; if more than 64 MiB is written before `rcctl` attaches, it fails fast — use `rcctl cp` for file transfers.
- Each command creates a `WorkspaceExec` resource that expires 10 minutes after completion. The Workspace's `rc-kube` supervisor retains execution history (90 days and 15,000 entries by default, configurable via `spec.executionRetention`).

```sh
rcctl -n development ps                    # Running processes in this namespace
rcctl -n development ps -a                 # Include finished processes
rcctl -n development ps --workspace dev    # Query one Workspace

# Replace PROCESS_ID with an ID from `rcctl ps`:
# rcctl -n development inspect PROCESS_ID
# rcctl -n development logs PROCESS_ID
# rcctl -n development attach PROCESS_ID
# rcctl -n development stop PROCESS_ID

# Copy files to/from a running Workspace:
# rcctl -n development cp ./local.txt dev:/workspace/rc/local.txt
# rcctl -n development cp dev:/workspace/rc/artifacts ./artifacts
```

## Caching npm packages

Point `--npm-registry` at an in-cluster npm registry or caching proxy (such as Verdaccio) on `workspace create`, `run`, or `exec`:

```sh
rcctl workspace create rc-dev \
  --npm-registry http://verdaccio.rc-system.svc.cluster.local:4873
```

This sets `NPM_CONFIG_REGISTRY` (with a trailing slash) and `COREPACK_NPM_REGISTRY` (without a trailing slash). On `exec`, it scopes the override to that execution only. It conflicts with setting either variable explicitly via `--env` / `--env-file` and does not configure registry authentication.
