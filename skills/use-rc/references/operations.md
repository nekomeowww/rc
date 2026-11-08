# rc operations

Use these patterns after the entrypoint has resolved the connection, context, namespace, and installed CLI capabilities.

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
rcctl -n <namespace> exec --no-env-passthrough --credential github-ssh <workspace> -- git ls-remote git@github.com:<owner>/<repository>.git HEAD
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
rcctl -n <namespace> exec --no-env-passthrough --cwd /workspace/<mount> <workspace> -- <command> <args...>
rcctl -n <namespace> exec --no-env-passthrough -it --agent-credential codex --cwd /workspace/<mount> <workspace> -- codex
rcctl -n <namespace> exec --no-env-passthrough -d --cwd /workspace/<mount> <workspace> -- <server-or-watcher> <args...>
rcctl -n <namespace> run --no-env-passthrough --rm --environment <environment> -- <bounded-command> <args...>
```

Foreground execution waits for completion. Add `-it` to forward input and allocate
a terminal, or `-d` to start detached. Without `-i` the command reads end of file
on standard input, so prompts fail instead of waiting. With `-i` (no `-t`), piped
input reaches the command and its end closes the command's input. A foreground
`exec` receives all output; if more than 64 MiB is written before rcctl attaches,
rcctl exits with an error instead of returning partial output.

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

### Copy files

Use `rcctl cp` to move files, scripts, screenshots, or build output. Do not
pass file contents through command arguments or `exec` output: arguments are
limited to about 128 KiB, and `exec` output is not binary safe.

```sh
rcctl -n <namespace> cp ./script.sh <workspace>:/workspace/<mount>/script.sh
rcctl -n <namespace> cp <workspace>:/workspace/<mount>/screenshots ./screenshots
```

The Workspace must be running. A relative remote path starts at `/workspace`,
which only holds mounts; copy into a mount or the home directory.
