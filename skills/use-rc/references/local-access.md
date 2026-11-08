# Local access configuration and migration

Device-specific values are local data, not public skill instructions. Reuse the
user's existing kubeconfig, CLI configuration, secret manager, and service-manager
configuration before introducing another file. Read only the selected profile;
do not scan unrelated secrets or execute arbitrary shell text from a profile.

When a local profile is useful, recommend Linux-like XDG paths on Linux and
macOS: `${XDG_CONFIG_HOME:-$HOME/.config}/rc/` for private access metadata and
`${XDG_STATE_HOME:-$HOME/.local/state}/rc/` for logs/PIDs. These are conventions for
agents, not a new rcctl configuration format or automatically loaded CLI flags.
Use native owner-only ACLs and user-private paths on Windows.

A profile may describe the access device, kubeconfig path, context, namespace,
route kind (direct/TCP/SOCKS), Access hostname, authentication mode, local bind,
TLS identity, 1Password account/reference mapping, and an existing supervisor's
service identity. Preserve existing configuration formats. Do not put secret
values or arbitrary executable hooks in it. Account IDs, references, and internal
hostnames are private metadata too; do not publish them in examples or dotfiles.

## Retire a private use-rcctl skill

1. Inventory the old entrypoint, references, scripts, service definitions, and
   their callers without printing secret contents. Separate reusable procedures
   from device facts and credential values.
2. Move only reusable guidance to public skills; keep actual hostnames, account
   IDs, resource names, service identities, and credential locations local.
   Existing authorized services can remain in use; moving documentation does not
   require replacing working tunnels or re-reading cached secrets.
3. For any newly persisted secret, follow the persistence section in
   [the secret-access skill](../../use-op-for-secret-access/SKILL.md). Use an
   existing approved provider if 1Password is not used; do not require installing it.
4. Install `use-rc` and the companions the device needs. Verify the selected route,
   Kubernetes identity/RBAC, and a requested rc operation before retiring the old
   instructions. Verify meaningful exec/attach behavior if the workflow needs it;
   plain API reads alone do not prove streaming connections work.
5. Replace local `use-rcctl` references in agent rules with `use-rc`. Remove the
   obsolete installed skill only after checking that no service still depends on
   its scripts; relocate such scripts and update their callers first. Preserve
   credentials, existing profiles, and unrelated rules. Removing a skill does not
   revoke credentials or stop its previously launched processes.

A public PR can provide the migration procedure but cannot migrate every user's
private device state. Report which local migration steps actually ran.
