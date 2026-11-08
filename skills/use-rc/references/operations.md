# rc command patterns

Use the selected config/context consistently; replace placeholders and inspect installed help for release differences. Existing authorization applies; credential imports grant access to the target namespace and require that scope.

```sh
rcctl -n <namespace> env list
rcctl -n <namespace> repo list
rcctl -n <namespace> worktree list
rcctl -n <namespace> workspace list
rcctl -n <namespace> ps -a --workspace <workspace>
```

## Credentials

```sh
gh auth status --hostname github.com
rcctl -n <namespace> credentials import --type github --name github-com
rcctl -n <namespace> credentials import --type agent --agent codex --file "${CODEX_HOME:-$HOME/.codex}/auth.json"
```

Git credentials serve Repository clone/sync; application auth inside a Workspace is separate. Workspace credential references are an allowlist: select credentials per execution. The default `rc-workspace` ServiceAccount permits same-namespace rc management, not cluster-wide access. Process credential projection does not isolate hostile peers sharing a Workspace.

For SSH credentials, inspect the Credential schema and clone consumer first: Repository Jobs do not use Workspace SSH fragments. Verify host keys, scope aliases per key, and preserve Secret references; prefer authorized HTTPS Git access where available.

## Code and execution

```sh
rcctl -n <namespace> repo clone <git-url> --name <repository> --storage-class <clone-capable-class>
rcctl -n <namespace> repo sync <repository>
rcctl -n <namespace> worktree add --repo <repository> --name <worktree> --branch <branch>
rcctl -n <namespace> workspace create <workspace> --environment <environment> --cwd /workspace/<mount>
rcctl -n <namespace> workspace mount worktree <worktree> --workspace <workspace> --name <mount> --path <mount>
rcctl -n <namespace> exec --no-env-passthrough --cwd /workspace/<mount> <workspace> -- <command>
rcctl -n <namespace> exec --no-env-passthrough -d <workspace> -- <server>
rcctl -n <namespace> exec --no-env-passthrough -it --agent-credential codex <workspace> -- codex
rcctl -n <namespace> run --no-env-passthrough --rm --environment <environment> -- <command>
```

Flags precede the positional Workspace; the program follows `--`. `-i` forwards stdin, `-t` allocates a terminal, and foreground execution waits. Do not mutate mounts while relying on existing runtime processes.

```sh
rcctl -n <namespace> inspect <execution>
rcctl -n <namespace> logs <execution>
rcctl -n <namespace> attach <execution>
rcctl -n <namespace> stop <execution>
rcctl -n <namespace> rm <completed-execution>
rcctl -n <namespace> cp ./script.sh <workspace>:/workspace/<mount>/script.sh
rcctl -n <namespace> cp <workspace>:/workspace/<mount>/screenshots ./screenshots
```

Attach reconnects, not reruns. Use `cp` for binary/large files; command arguments and exec output are unsuitable. The Workspace must run; remote relative paths start at `/workspace`, so copy into a mount or home. Before stopping/deleting resources, check ownership, uncommitted work, consumers, and retention.
