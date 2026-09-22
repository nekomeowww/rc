---
name: use-rc
description: Operate rc Kubernetes development environments with rcctl. Use when the user asks to use rc or rcctl, or needs persistent remote coding Workspaces, Git Worktrees, or supervised processes. Do not use merely because Kubernetes is available.
---

# Use rc

Use rc as a Kubernetes-backed development runtime. Treat Repository mirrors, writable Worktrees, persistent Workspaces, and supervised WorkspaceExecs as separate resources.

## Before operating

- Prefer the released CLI: `go install github.com/nekomeowww/rc/cmd/rcctl@latest`. Confirm `rcctl` is on `PATH`; use a checkout build only when unreleased source changes are requested. Installing it changes the local host, so propose the command and obtain explicit approval if it is absent.
- Resolve kubeconfig exactly as `kubectl` would. Inspect the selected context, namespace, rc CRDs/controller, StorageClasses, and current rc resources before proposing a mutation. Determine the namespace from an explicit user choice or read-only context/resource discovery; do not assume `default` when it is unspecified. Never print kubeconfig or secret contents.
- If no usable kubeconfig/cluster is available, explain that rc needs one and ask the user to prepare or select a cluster. Do not create or change a cluster without explicit approval. For a disposable local cluster, route to `$setup-rc-locally`.
- If rc is absent on the selected cluster, route to `$setup-rc`. Installing it and importing credentials are mutations and require explicit approval immediately before they occur.

## Select the execution shape

- Use `repo exec` or `worktree exec` only for short, non-interactive work.
- Use `rcctl exec [flags] WORKSPACE -- COMMAND` for an existing Workspace. The positional Workspace is required even when `workspace default` is configured; put rcctl flags before it.
- Use `rcctl run [flags] -- COMMAND` to create a new Workspace. It is retained by default; add `--name` for a stable name or `--rm` for a bounded isolated task whose home and generated Worktree should be cleaned up after all executions terminate. Referenced user-created Worktrees are not owned by this cleanup.
- `env default` supplies the Environment for new Workspace creation with `run`; it does not select or replace the target of `exec`.
- Default to reusing a suitable named Workspace for the same project and trust boundary. One Workspace is designed to mount multiple Worktrees and run multiple WorkspaceExecs concurrently, so mount the project's additional branches there and start each agent, server, watcher, or command as a separate process.
- A Repository is the shared mirror, not a writable checkout. Write in a Worktree. A read-write Worktree has one Workspace/WorktreeExec owner at a time.
- Create another Workspace only when isolation or incompatible requirements justify it: a different trust/credential boundary, OS/image/ServiceAccount, compute or node placement, independent lifecycle, exclusive Worktree ownership, or processes that cannot share the Pod's resources or network namespace (for example, conflicting fixed ports). Mount changes replace the shared Workspace Pod; account for active processes first.

## Credentials, access, and processes

- Scope Git credentials to Repository clone and sync operations. An imported GitHub CLI credential does not authenticate `gh` or Git inside a Workspace.
- Workspace credential references are an allowlist; select required process credentials explicitly on every WorkspaceExec. Do not put secrets in images, Environment snapshots, command arguments, source, or logs.
- A Workspace is one trust boundary: its processes share a Pod, home, mounts, network identity, and effective access. Use a separate Workspace for untrusted work or a different secret set.
- rc normally injects the same-namespace `rc-workspace` ServiceAccount into a Workspace. It permits rc and related Kubernetes operations in that namespace, so software launched in a Workspace or WorkspaceExec can programmatically use `rcctl` to manage same-namespace rc resources. It does not grant unrelated cluster-wide privileges. Use `--no-service-account` or an explicitly chosen same-namespace ServiceAccount only when requested.
- The execution CR is `WorkspaceExec` (`workspaceexecs.workspaces.rc.ayaka.io`). Execution commands live at the CLI root; there is no `agent` or `workspace exec` command group. AgentCredentials and their `--agent-credential` flag remain separate from execution naming.
- WorkspaceExecs are supervised and at-most-once. Client disconnect does not stop them. Runtime loss marks live executions `Lost`; rc does not automatically rerun them. Use rc's detach, attach, logs, and stop lifecycle rather than `nohup`, `tmux`, or application daemon modes.

## Basic command patterns

Replace every angle-bracket value with the selected context, namespace, and
resource names. Inspecting is read-only; cloning, creating, mounting, and
starting processes require authorization for the requested resources.

### Inspect a namespace

```sh
rcctl -n <namespace> env list
rcctl -n <namespace> repo list
rcctl -n <namespace> worktree list
rcctl -n <namespace> workspace list
rcctl -n <namespace> ps -o wide
```

`ps` lists running executions in the selected namespace. Add `--workspace <workspace>` to filter them, `-a` to include pending/completed records, or `-A` to span permitted namespaces. Use `workspace list` for Workspaces.

### Import Git credentials

These commands create namespaced Secrets and rc credential resources. First
confirm the source credential exists without displaying it, state the target
namespace and resource names, and obtain explicit approval.

For GitHub HTTPS clone/sync authentication, import the token held by an already
authenticated local GitHub CLI. This Credential is for Repository operations;
it does not authenticate `gh` inside a Workspace.

```sh
gh auth status --hostname github.com
rcctl -n <namespace> credentials import --type github --name github-com
```

For SSH Git remotes, create an `SSHPrivateKey` Credential from a private-key
Secret and a verified `known_hosts` file. `rcctl credentials import` does not
yet import this credential type directly. Obtain GitHub host keys from a
trusted, independently verified source. This example uses `ssh.github.com:443`.
The `known_hosts` entry must cover `[ssh.github.com]:443`.

```sh
kubectl -n <namespace> create secret generic <github-ssh-secret> --from-file=ssh-privatekey=<private-key-file> --from-file=known_hosts=<verified-known-hosts-file>
```

Apply this Credential manifest, preserving the literal placeholder strings:

```yaml
apiVersion: configs.rc.ayaka.io/v1alpha1
kind: Credential
metadata:
  name: github-ssh
spec:
  type: SSHPrivateKey
  sshPrivateKey:
    privateKeyRef:
      name: <github-ssh-secret>
      key: ssh-privatekey
    knownHostsRef:
      name: <github-ssh-secret>
      key: known_hosts
    config: |
      Host github.com
        HostName ssh.github.com
        Port 443
        User git
        IdentityFile ${identityFile}
        UserKnownHostsFile ${knownHostsFile}
        IdentitiesOnly yes
```

Workspace processes use `config` through rc-managed SSH fragments.
Repository clone Jobs do not read it. For clone, use
`ssh://git@ssh.github.com:443/<owner>/<repository>.git` with `--credential-ref github-ssh`.

Before reuse, inspect the Credential configuration:

```sh
kubectl -n <namespace> get credential github-ssh -o jsonpath='{.spec.sshPrivateKey.config}'
```

If configuration is missing or incorrect, correct the manifest without changing its Secret references.
For shared credentials, check existing consumers before changing host matching.
Add the Credential to the Workspace allowlist and select it on each execution.
Verify access to the intended private repository with ordinary Git:

```sh
rcctl -n <namespace> exec --credential github-ssh <workspace> -- git ls-remote git@github.com:<owner>/<repository>.git HEAD
```

#### Multiple SSH keys

Use one Credential per key and a distinct `Host` alias per identity.
Keep `HostName ssh.github.com`, `Port 443`, and the placeholders in each fragment.

| Credential | Fragment `Host` | Workspace Git remote |
| --- | --- | --- |
| `github-personal` | `github-personal` | `git@github-personal:<owner>/<repository>.git` |
| `github-work` | `github-work` | `git@github-work:<owner>/<repository>.git` |
| `github-bot` | `github-bot` | `git@github-bot:<owner>/<repository>.git` |
| `github-deploy` | `github-deploy` | `git@github-deploy:<owner>/<repository>.git` |

Select credentials with repeated `--credential` flags and use the matching alias in each Workspace remote.
Keep aliases distinct across concurrent processes because they share SSH configuration.
Matching `IdentityFile` entries accumulate, so one shared `Host` pattern cannot select different identities by repository path.

### Import Codex auth credential

To make the local Codex login selectable by Codex WorkspaceExecs, import its
auth file as an AgentCredential. Do not print or copy its contents; add
`--agent-credential codex` when creating the Workspace or process that needs
it.

```sh
test -f "${CODEX_HOME:-$HOME/.codex}/auth.json"
rcctl -n <namespace> credentials import --type agent --agent codex --file "${CODEX_HOME:-$HOME/.codex}/auth.json"
```

### Create a project Workspace and its primary checkout

Use an existing compatible Environment and a clone-capable StorageClass. Add
`--credential-ref <git-credential>` to a private HTTPS clone only when that
Credential has already been approved and imported.

```sh
rcctl -n <namespace> repo clone <git-url> --name <repository> --storage-class <clone-capable-storage-class>
rcctl -n <namespace> workspace create <workspace> --environment <environment> --cwd /workspace/<mount>
rcctl -n <namespace> workspace mount repo <repository> --workspace <workspace> --name <mount> --path <mount>
```

### Update the parent Repository

Before creating a Worktree that needs current remote code:

```sh
rcctl -n <namespace> repo sync <repository>
```

Sync fetches and resets the parent using its configured ref and Credential. It
removes untracked parent files and leaves existing Worktrees unchanged. It waits
for parent writers, pending clones, and direct Repository mounts. Suspend direct
parent mounts first. Workspaces mounted on independent Worktrees can keep running.
Use `--wait=false` to submit without waiting; the command prints the request name.
Completed request records expire after 3 days by default, on success or failure.

### Add a parallel branch to the same Workspace

Prefer this over another Workspace when the branches intentionally share home
state, credentials, compute, and lifecycle:

```sh
rcctl -n <namespace> worktree add --repo <repository> --name <feature-worktree> --branch <branch>
rcctl -n <namespace> workspace mount worktree <feature-worktree> --workspace <workspace> --name <feature-mount> --path <feature-mount>
```

Mount changes replace the Workspace Pod, so perform them before starting
long-lived processes whenever possible.

### Run one-off and persistent work

```sh
rcctl -n <namespace> exec --cwd /workspace/<mount> <workspace> -- <command> <args...>
rcctl -n <namespace> exec -it --agent-credential codex --cwd /workspace/<mount> <workspace> -- codex
rcctl -n <namespace> exec -d --cwd /workspace/<mount> <workspace> -- <server-or-watcher> <args...>
rcctl -n <namespace> run --rm --environment <environment> -- <bounded-command> <args...>
```

Foreground execution waits for completion. Add `-it` to forward input and allocate
a terminal, or `-d` to start detached.

```sh
rcctl -n <namespace> ps -a --workspace <workspace> -o wide
rcctl -n <namespace> inspect <execution-id>
rcctl -n <namespace> logs <execution-id>
rcctl -n <namespace> attach <execution-id>
rcctl -n <namespace> stop <execution-id>
rcctl -n <namespace> rm <completed-execution-id>
```

`attach` reconnects to the same live execution; it does not rerun a completed or
Lost command. `rm` deletes a completed execution record. Deleting the Workspace
is a separate operation through `workspace delete`.

## pnpm caches in Linux Workspaces

Keep source on PVC and rebuildable dependencies in local storage. Updated Linux
images use `XDG_CACHE_HOME=/tmp/cache`; check this and directory permissions for
older/custom images.

```sh
rcctl -n <namespace> exec --cwd /workspace/<mount> <workspace> -- pnpm install --store-dir=/tmp/cache/pnpm/store --virtual-store-dir=/tmp/cache/pnpm/virtual/<worktree-id>
```

- Use the Worktree resource name for `<worktree-id>`. Share the content store
  within a Workspace, but give each Worktree its own virtual store.
- Install from the repository's required root and follow its pnpm, lockfile, and
  build-script policies. Keep both path flags for `install/add/remove/update`;
  pnpm 11+ does not read store settings from `.npmrc`.
- Serialize dependency changes per Worktree. All consuming processes must see
  the same virtual-store path.
- Local caches can disappear when the runtime is replaced. Keep credentials out
  of them and never clear dependencies while processes are using them.

### Cache recovery

After runtime replacement, check dependency links: `Already up to date` does not
guarantee a missing virtual store was rebuilt. For stale links or path migration,
stop affected processes, move only that Worktree's generated root/package
`node_modules` directories to a unique PVC backup outside workspace package globs,
then reinstall with the same flags and verify runtime resolution. Preserve source
and lockfiles; remove the identified backups only when authorized.

rc does not automate recovery. Avoid defaulting to `--force`, which can fetch
other-platform optional dependencies. For durability instead, select persistent
content and virtual stores, accepting PVC I/O costs.

See [pnpm virtualStoreDir](https://pnpm.io/settings/node-modules#virtualstoredir).

## Typical sequence

Inspect first, then create only requested resources: namespace; credentials; Environment or compatible runner; Repository; Workspace; writable Worktree mount; initialization process; then separate WorkspaceExecs for each agent, server, or watcher. Use installed `rcctl --help` as the command contract because the API is currently `v1alpha1` and flags can change.

For LobeHub CLI login or `lh connect`, use `$setup-lobehub-cli`. Do not copy a host-created LobeHub credential file into a Workspace: its encryption is tied to host/user identity. Log in once inside a persistent Workspace instead.
