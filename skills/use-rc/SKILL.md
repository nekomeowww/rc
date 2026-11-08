---
name: use-rc
description: Use rcctl for remote development Workspaces, writable Git Worktrees, and supervised processes. Compose optional credential and connectivity skills when the selected cluster needs them.
---

# Use rc

1. Identify the access device and reuse its kubeconfig, context, namespace, and working route. Never assume namespace `default`. Check installed CLI help and controller compatibility; inspect selected resources before mutations. Never dump credentials or assume a deployment's names.
2. Load only the needed companion (linked repository copies work if not installed):
   - 1Password reads: [use-op-for-secret-access](../use-op-for-secret-access/SKILL.md), supplying account, references, consumer, and authorization scope.
   - Cloudflare route: [use-cloudflared-for-remote-access](../use-cloudflared-for-remote-access/SKILL.md), supplying endpoint, upstream protocol, auth mode, and lifetime.
   - Context edits: [kubeconfig](../use-cloudflared-for-remote-access/references/kubeconfig.md).
   - Device profiles or retiring a private skill: [local access](references/local-access.md).
3. Verify transport, Kubernetes identity, then namespace-specific rc access. Anonymous `/version` success is insufficient; Forbidden may be RBAC. Missing cluster/installations route to `$setup-rc-locally` or `$setup-rc` within the user's authorized scope.

## Execute

- A Repository is a mirror; develop in an independently owned writable Worktree. Sync before branching from current remote code; sync resets the parent and removes untracked files.
- Reuse a compatible Workspace within the same trust boundary. Split for isolation, incompatible image/compute/lifecycle, or port conflicts. Processes share home, mounts, network, and access. Mount changes replace the Pod: inspect running work first.
- `exec [flags] WORKSPACE -- COMMAND` needs an existing positional Workspace. `run` creates one: named runs retain; unnamed runs clean up unless `--retain`; check release semantics. `env default` chooses an Environment, not an exec target.
- On `exec`, `run`, and `env exec`, use `--no-env-passthrough` and select needed `--env`, `--credential`, and `--agent-credential` values. Access-device credentials stay local; Repository Git auth does not authenticate Workspace applications.
- Supervise each long-lived process separately with detached exec. Use `ps/inspect/logs/attach/stop`, not nested daemons. Disconnect does not stop execution; Lost executions require checking side effects before retry. Execution-record deletion does not delete a Workspace.

Read [operations](references/operations.md) for commands and [storage/caches](references/storage-and-caches.md) when preparing storage or dependencies. For Workspace LobeHub login, use `$setup-lobehub-cli` rather than copying host-bound credentials.

Report selected config/context/namespace, retained resources, verified outcome, and any tunnel lifetime/blocker. Readiness or process start alone is not application verification.
