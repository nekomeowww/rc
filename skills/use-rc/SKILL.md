---
name: use-rc
description: Operate rc Kubernetes development environments with rcctl, composing existing cluster access and optional secret-manager or Cloudflare connectivity. Use for rc Workspaces, Git Worktrees, and supervised processes; not merely because Kubernetes is available.
---

# Use rc

Use rc as a Kubernetes-backed development runtime. Keep Repository mirrors,
writable Worktrees, persistent Workspaces, and supervised WorkspaceExecs distinct.

## Resolve access and compose only what is needed

1. Identify the access device running kubectl/rcctl. It is distinct from the
   Kubernetes nodes and Workspace runtime. Loopback, credential files, and tunnels
   belong to that device. Do not copy credentials to another device for convenience.
2. Read repository instructions and inspect installed `rcctl --help`, `rcctl exec
   --help`, and relevant subcommand help. Prefer a released CLI compatible with the
   controller. If installation is needed, use the authorized installation workflow;
   a source build is for requested unreleased changes.
3. Resolve kubeconfig as kubectl would, preserving the user's selected file,
   context, namespace, CA, TLS name, authentication, and proxy settings. Set the
   selected config/context consistently for each new shell and both clients.
   Inspect metadata without dumping raw kubeconfig, tokens, or exec-plugin secrets.
4. Reuse existing working access. Read [local access configuration](references/local-access.md)
   when resolving device-specific settings or replacing a private local skill.
   Do not assume a cluster hostname, secret provider, proxy port, or service manager.
5. If an authorized credential must be read from 1Password, invoke
   `$use-op-for-secret-access` ([entrypoint](../use-op-for-secret-access/SKILL.md)).
   Pass the selected account, exact secret references, consuming process, and
   authorization scope. Skip it for existing credentials or another provider.
6. If Cloudflare is actually in the route, invoke
   `$use-cloudflared-for-remote-access` ([entrypoint](../use-cloudflared-for-remote-access/SKILL.md)).
   Pass the intended endpoint, known upstream protocol, authentication mode, and
   required lifetime. Connecting to a remote API does not authorize publishing a
   local service. Skip this skill for direct or other working access.
7. If a named companion is not installed, read the linked repository copy if
   available, or explain which companion to install; do not silently invent its
   authentication procedure. Helpers return configuration/file handles and
   verification evidence, never secret values in conversation.
8. Confirm transport, Kubernetes identity, and permission separately with bounded
   read-only requests, then list rc resources in the intended namespace. A version
   response may permit anonymous access; Forbidden can mean limited RBAC rather
   than broken connectivity. Do not require cluster-wide discovery to succeed.

If no usable cluster is available, ask the user to select or prepare one. Use
`$setup-rc-locally` for a requested disposable cluster and `$setup-rc` for an
installation on the selected cluster. Existing task authorization persists;
obtain authorization for additional cluster installation or credential import
when it is outside that scope.

## Choose resources and execution

- Reuse a suitable named Workspace for the same project and trust boundary.
  Inspect active processes, image/tools, free resources, ownership, mounts, and
  uncommitted work first. Add independent writable Worktrees for independent writers.
- A Repository is a shared mirror. Sync it before creating a Worktree needing
  current remote code; Ready does not prove freshness. Sync resets the parent and
  removes its untracked files, so inspect consumers first.
- Use `repo exec` or `worktree exec` for short non-interactive work; use
  `rcctl exec [flags] WORKSPACE -- COMMAND` for an existing Workspace. Its positional
  Workspace is required even when a default is configured.
- `env default` chooses an Environment for new Workspace creation; it does not
  select the existing Workspace targeted by `exec`.
- `rcctl run` creates a Workspace. In the current CLI, unnamed runs clean up five
  minutes after executions end; named runs retain by default. Choose `--retain`
  for later exec/results or `--rm` for explicit cleanup. Check installed help for
  actual release semantics. User-created referenced Worktrees are not run-owned.
- Split Workspaces for a different trust/credential boundary, image/OS,
  ServiceAccount, compute placement, lifecycle, exclusive mount, or incompatible
  ports. Processes in one Workspace share Pod, home, network, mounts, and access.
- Mount topology changes replace the Pod. Plan mounts before long-lived processes
  and inspect running work before changing them. Do not bypass Worktree ownership.

Read [operation patterns](references/operations.md) for credential import,
Repository sync, Worktree mounting, execution, and file copying. Read
[storage and caches](references/storage-and-caches.md) before selecting storage,
placing dependency caches, or recovering dependencies after runtime replacement.

## Credentials and supervision

- Disable caller environment pass-through on `exec`, `run`, and `env exec` using
  `--no-env-passthrough`; select only needed values with supported `--env` flags.
  Credentials used to reach rc, such as Access tokens, must stay on the access
  device. Check installed help if these flags are missing; do not silently forward
  the caller's whole environment.
- Repository Git credentials, application login in a Workspace, and access-device
  login are separate. Workspace credential references are an allowlist; explicitly
  select needed `--credential` and `--agent-credential` values per execution.
- rc normally injects the same-namespace `rc-workspace` ServiceAccount. It grants
  same-namespace rc operations, not unrelated cluster-wide access. Change this only
  within the user's requested scope. Process credential projection is not isolation
  from hostile peers in the same Workspace.
- Run each server, agent, or watcher as its own supervised detached WorkspaceExec.
  Use root-level `ps`, `inspect`, `logs`, `attach`, and `stop`; there is no `agent`
  or `workspace exec` command group. Access-device tunnels have their own lifecycle.
- Executions are at-most-once: client disconnect does not stop them; runtime loss
  can mark them Lost. Check side effects before rerunning. Deleting a completed
  execution record is separate from deleting its Workspace or persistent storage.
- For LobeHub login inside a persistent Workspace, use `$setup-lobehub-cli`.
  Do not copy host-bound encrypted application credentials between devices.

## Return a verifiable handoff

Report the selected device/config/context/namespace, route and authentication
mode, identity and permitted operation evidence, retained Workspace/Worktree and
execution identifiers, and the requested behavior's verification result. Include
any owned tunnel's lifetime/stop method and blockers. Keep credentials and private
environment details out of published artifacts. A listening socket, Ready Pod,
or started execution alone does not verify the requested application behavior.
