# Local access and migration

Reuse existing kubeconfig, secret storage, and supervision. Keep device-specific endpoints, account/reference mappings, and service identities private; public skills describe procedures, not deployments.

If a profile is needed, use `${XDG_CONFIG_HOME:-$HOME/.config}/rc/` for metadata and `${XDG_STATE_HOME:-$HOME/.local/state}/rc/` for logs/PIDs, including on macOS. These are agent conventions, not rcctl flags or an executable profile format. Windows uses private user paths/ACLs.

To retire a private `use-rcctl`:

1. Inventory its callers, scripts, services, and credentials without displaying secrets. Move reusable guidance into public skills and deployment facts into private configuration.
2. Install `use-rc` and needed companions; verify the route, Kubernetes identity/RBAC, rc operation, and streaming if required.
3. Relocate any scripts still used by services before deleting the old skill; update local agent-rule references. Preserve working credentials/services and unrelated rules. Removing a skill neither stops processes nor revokes credentials.

For newly cached secrets, follow [secret persistence](../../use-op-for-secret-access/SKILL.md). A repository PR supplies migration instructions; report local migration only if actually performed.
