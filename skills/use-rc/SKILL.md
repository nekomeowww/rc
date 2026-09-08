---
name: use-rc
description: Operate rc Kubernetes development environments with rcctl. Use when the user asks to use rc or rcctl, or needs persistent remote coding Workspaces, Git Worktrees, or supervised agent processes. Do not use merely because Kubernetes is available.
---

# Use rc

Use rc as a Kubernetes-backed development runtime. Treat Repository mirrors, writable Worktrees, persistent Workspaces, and supervised AgentProcesses as separate resources.

## Before operating

- Prefer the released CLI: `go install github.com/nekomeowww/rc/cmd/rcctl@latest`. Confirm `rcctl` is on `PATH`; use a checkout build only when unreleased source changes are requested. Installing it changes the local host, so propose the command and obtain explicit approval if it is absent.
- Resolve kubeconfig exactly as `kubectl` would. Inspect the selected context, namespace, rc CRDs/controller, StorageClasses, and current rc resources before proposing a mutation. Determine the namespace from an explicit user choice or read-only context/resource discovery; do not assume `default` when it is unspecified. Never print kubeconfig or secret contents.
- If no usable kubeconfig/cluster is available, explain that rc needs one and ask the user to prepare or select a cluster. Do not create or change a cluster without explicit approval. For a disposable local cluster, route to `$setup-rc-locally`.
- If rc is absent on the selected cluster, route to `$setup-rc`. Installing it and importing credentials are mutations and require explicit approval immediately before they occur.

## Select the execution shape

- Use `repo exec` or `worktree exec` only for short, non-interactive work.
- Use `agent exec/run --temporary` for a bounded isolated task whose home and generated Worktree may be removed after completion.
- Default to reusing a suitable named Workspace for the same project and trust boundary. One Workspace is designed to mount multiple Worktrees and run multiple AgentProcesses concurrently, so mount the project's additional branches there and start each agent, server, watcher, or command as a separate process.
- A Repository is the shared mirror, not a writable checkout. Write in a Worktree. A read-write Worktree has one Workspace/WorktreeExec owner at a time.
- Create another Workspace only when isolation or incompatible requirements justify it: a different trust/credential boundary, OS/image/ServiceAccount, compute or node placement, independent lifecycle, exclusive Worktree ownership, or processes that cannot share the Pod's resources or network namespace (for example, conflicting fixed ports). Mount changes replace the shared Workspace Pod; account for active processes first.

## Credentials, access, and processes

- Scope Git clone credentials to the Repository clone operation. An imported GitHub CLI credential does not authenticate `gh` or Git inside a Workspace.
- Workspace credential references are an allowlist; select required process credentials explicitly on every AgentProcess. Do not put secrets in images, Environment snapshots, command arguments, source, or logs.
- A Workspace is one trust boundary: its processes share a Pod, home, mounts, network identity, and effective access. Use a separate Workspace for untrusted work or a different secret set.
- rc normally injects the same-namespace `rc-workspace` ServiceAccount into a Workspace. It permits rc and related Kubernetes operations in that namespace, so software launched in a Workspace or AgentProcess can programmatically use `rcctl` to manage same-namespace rc resources. It does not grant unrelated cluster-wide privileges. Use `--no-service-account` or an explicitly chosen same-namespace ServiceAccount only when requested.
- AgentProcesses are supervised and at-most-once. Use rc's detach, resume, log, and stop lifecycle rather than `nohup`, `tmux`, or application daemon modes.

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
rcctl -n <namespace> agent list -o wide
```

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
trusted, independently verified source; for GitHub SSH over port 443 the
`known_hosts` entry must cover `[ssh.github.com]:443`.

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

Use `git@github.com:<owner>/<repository>.git` with `--credential-ref github-ssh`
for an SSH Repository clone, and explicitly select `--credential github-ssh`
on each AgentProcess that needs authenticated Git operations.

### Import Codex auth credential

To make the local Codex login selectable by Codex AgentProcesses, import its
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
rcctl -n <namespace> agent exec --workspace <workspace> --cwd /workspace/<mount> -- <command> <args...>
rcctl -n <namespace> agent run -d --workspace <workspace> --cwd /workspace/<mount> -- <server-or-watcher> <args...>
rcctl -n <namespace> agent exec --temporary --environment <environment> -- <bounded-command> <args...>
```

Use `agent list`, `agent logs <process-id>`, `agent resume <process-id>`, and
`agent stop <process-id>` for the lifecycle of detached or interactive work.

## Typical sequence

Inspect first, then create only requested resources: namespace; credentials; Environment or compatible runner; Repository; Workspace; writable Worktree mount; initialization process; then separate AgentProcesses for each agent, server, or watcher. Use installed `rcctl --help` as the command contract because the API is currently `v1alpha1` and flags can change.

For LobeHub CLI login or `lh connect`, use `$setup-lobehub-cli`. Do not copy a host-created LobeHub credential file into a Workspace: its encryption is tied to host/user identity. Log in once inside a persistent Workspace instead.
