---
name: use-op-for-secret-access
description: Read authorized secrets with 1Password CLI, select among accounts, inject credentials into a consuming process, or arrange explicitly authorized local persistence. Use for first-time or renewed secret access, not every invocation of an already configured CLI.
---

# Use 1Password for secret access

Resolve an authorized secret reference for a specific consumer without exposing
its value in conversation, logs, command arguments, or source. This skill can
serve rcctl, kubectl, AWS/GCP CLIs, cloudflared, and other consumers; it does not
choose their target environment or grant additional access.

## Inspect and select the account

1. Establish the requested references, consumer, target, and whether access is
   one-time or covered by existing authorization. Reuse valid consumer-native
   credentials or an approved cache instead of reopening 1Password each time.
2. Check `op --version`, `op account list --help`, `op read --help`, and `op run
   --help` before version-sensitive commands. If absent, use an authorized install
   path or report the missing prerequisite; do not build a replacement vault.
3. Inspect configured account metadata locally with `op account list`. Do not
   publish email addresses, sign-in addresses, account IDs, or vault inventories.
   Select the intended account explicitly with `--account <account-id>` rather
   than changing the global default or relying on whichever account signed in last.
4. If the account is unknown, use the user's intended personal/work scope to
   identify candidates. Try the exact authorized reference with explicit selectors
   among those candidates, sequentially, without enumerating unrelated vaults or
   triggering a batch of login prompts. Ask when the intended identity is ambiguous.
   A reference can exist in more than one account; first success is not enough to
   establish that it belongs to the intended environment.
5. Check that authentication works for the selected identity. Desktop integration,
   interactive session sign-in, 1Password Service Accounts, and Connect are distinct
   authentication modes; account selection does not override a Service Account or
   Connect token's scope. Do not replace a machine identity with a human login
   automatically. For macOS integration failures, read
   [macOS troubleshooting](references/macos.md).

A non-disclosing existence/access probe, after authorization, is:

```sh
op read --account <account-id> 'op://<vault>/<item>/<field>' >/dev/null
```

Treat reference names and errors as private metadata. Distinguish account mismatch,
locked/unauthenticated session, permission denial, missing item, and missing field.
Stop repeated identical failures; do not try unrelated identities to bypass denial.

## Deliver to the consumer

Prefer `op run` with secret references in selected environment variables when the
consumer supports them. For a Cloudflare Access TCP listener, after checking the
installed commands' supported names:

```sh
TUNNEL_SERVICE_TOKEN_ID='op://<vault>/<item>/<client-id-field>' \
TUNNEL_SERVICE_TOKEN_SECRET='op://<vault>/<item>/<client-secret-field>' \
op run --account <account-id> -- cloudflared access tcp \
  --hostname <access-hostname> --url 127.0.0.1:<port>
```

The references are resolved for the subprocess; the secret values are not CLI
arguments. Do not use `set -x`, printenv, environment dumps, or shell substitution
that places resolved credentials in argv. Environment injection is not isolation:
processes with the same user or sufficient privileges may inspect it. Limit the
child environment and do not propagate access-device credentials to rc Workspaces.

If a consumer requires a file, use a private directory and `op read --out-file`
with mode `0600` on POSIX, or an equivalent owner-only Windows ACL. Avoid shared
`/tmp`, symlinks, existing-file overwrite, and printing contents; give the consumer
only the path. Remove temporary material through the owning workflow when it ends.
Do not persist kubeconfigs or cloud credentials merely to probe readability.

## Reusable launch procedures without plaintext caching

When the user wants a reusable script, save only the selected account, exact
references, target, and procedure in a private local script. Prefer one `op run`
around the long-lived consumer, resolving references once per process lifetime.
If `op read` is required, capture its output privately and inject only the selected
values into the child environment; do not use `eval`, secret-valued argv, or dump
captured output. Avoid an extra readability probe immediately before a read/run
that already proves access. Reuse a verified running listener instead of repeatedly
resolving credentials for each kubectl/rcctl request.

Distinguish agent authorization from 1Password unlock/approval. A saved procedure
can avoid asking the user the same scope question, but cannot promise that desktop
integration or an expired session will not request approval. Do not persist raw
`OP_SESSION_*` or a human master password to avoid prompts. For truly unattended
use, use a separately authorized provider-supported machine identity or credential
cache; storing a script does not grant that authorization. Do not silently change
accounts or downgrade auth when a future invocation needs an unlock.

The caller owns the consumer process tree. When wrapping a listener with `op run`
or a script, stopping only the wrapper PID may not stop every child: use an owned
session/process group or a supervisor with verified signal forwarding, and confirm
that the listener port closes on success, error, and interruption. Before deleting
private diagnostics on failure, classify the provider/consumer error and return a
sanitized actionable reason; do not replace an approval-required error with a
wrapper-process exception or discard the only evidence before classifying it.

## Offer persistence once, without blocking one-time use

At first authorized setup of a secret-backed CLI, ask whether the user wants to
remember access for future use. Explain the two separate choices:

- Remember account/reference mappings and the permitted consumer/scope. This
  records intent; it does not bypass 1Password's own unlock or approval controls.
- Cache resolved credentials for unattended use. This may permit access without
  another 1Password unlock and exposes bearer credentials to the local user,
  administrators, backups, or compromised software until expiry/revocation.

Continue authorized one-time use if the optional question is unanswered; do not
interpret silence as permission to cache plaintext or create a Service Account.
Reuse existing explicit persistence authorization within its scope. For unattended
work, prefer an already provisioned least-privilege machine identity or approved
cache. Do not weaken desktop security to simulate unattended access.

If caching is authorized:

1. Prefer the consumer's supported secure credential store. Otherwise use a
   dedicated directory under `${XDG_CONFIG_HOME:-$HOME/.config}/<consumer>/secrets/`
   on Linux and macOS. A private `.env` in that directory is acceptable when the
   user requests it. Avoid project `.env` or `.env.*` by default; an explicitly
   requested project location requires checking tracking/sync risks and the same
   owner-only controls. Use Linux-like XDG
   paths on macOS for this convention. Use a user-private native path on Windows.
2. Create POSIX directories `0700`, files `0600`, with restrictive umask before
   writing; verify ownership, reject symlinks, avoid overwriting unrelated files,
   and use atomic replacement for rotation. Never source a secret file as shell code.
3. Before writing, inspect `git config --global --get core.excludesFile` and the
   effective global excludes file, including Git's XDG default when unset. Add a
   narrow pattern for the secret directory if needed, preserving existing entries.
   Check with `git check-ignore --no-index` from any applicable dotfiles repository.
   Ignore rules do not protect already tracked files, forced adds, other sync tools,
   or backups: check tracking without displaying contents and stop if the location
   would be published. Do not automatically untrack or rewrite dotfiles history.
4. Record only non-secret metadata about the authorized target, consumer, scope,
   cache path, expiry if available, refresh/rotation, and removal/revocation steps.
   Caching does not extend expiry. Refresh an expired cache only within the recorded
   authorization; changed account, target, or scope needs renewed authorization.

For an existing or requested dotenv file consumed by cloudflared, follow
[credential input modes](../use-cloudflared-for-remote-access/references/credential-inputs.md).
A supplied credential file does not require opening 1Password again.

Return success/failure, selected identity privately, reference/file handles,
consumer, lifetime, and cleanup/refresh instructions. Never return the secret value.

See the official [read command](https://developer.1password.com/docs/cli/reference/commands/read/),
[`op run` environment injection](https://developer.1password.com/docs/cli/secrets-environment-variables/),
and [Service Accounts](https://developer.1password.com/docs/service-accounts/).
