---
name: use-op-for-secret-access
description: Read authorized 1Password secrets with explicit account selection, inject them into a consumer, or arrange requested persistence. Skip when existing credentials already suffice.
---

# Use 1Password for secret access

1. Confirm references, consumer, target, and existing authorization. Check installed `op` help; select the intended account from `op account list` with `--account <id>`, without changing global defaults. If unknown, try only relevant accounts against the exact reference; ambiguity or denial is not permission to search other vaults. Service Account/Connect scope is separate from a human account selector.
2. Prefer one `op run` per consumer lifetime; capture `op read` privately when needed. Do not print secrets, use secret-valued argv, `eval`, or redundant probes. Use private `0600` files only when required; environment injection is visible to sufficiently privileged local processes.
3. Save reusable launchers with account/references/procedure, not plaintext credentials or `OP_SESSION_*`. Reuse a running listener. Prior agent authorization avoids repeated scope questions, but cannot bypass provider unlock/approval; unattended access needs an authorized machine identity/cache.
4. Stop the whole owned consumer process tree, verify its port closes, and report a sanitized failure reason before deleting private diagnostics.

```sh
TUNNEL_SERVICE_TOKEN_ID='op://<vault>/<item>/<id-field>' \
TUNNEL_SERVICE_TOKEN_SECRET='op://<vault>/<item>/<secret-field>' \
op run --account <id> -- cloudflared access tcp \
  --hostname <endpoint> --url 127.0.0.1:<port>
```

## Persistence

Offer once at initial setup: remembering reference mappings differs from caching plaintext usable without another vault unlock. Explain exposure to local users/admins/backups and expiry/revocation; honor prior authorization. Silence means continue authorized one-time access, not cache secrets.

Prefer a native credential store or private `${XDG_CONFIG_HOME:-$HOME/.config}/<consumer>/secrets/` (Linux-like XDG on macOS). A requested `.env` is fine; avoid project files by default. Use owner-only directories/files (`0700`/`0600`, Windows equivalent), reject symlinks, write atomically, and preserve unrelated files. Check effective Git global ignores and dotfiles tracking before writing: ignores do not protect tracked files or other sync tools. Record scope, expiry, refresh and revocation without values; caching does not extend expiry.

Read [dotenv/credential modes](../use-cloudflared-for-remote-access/references/credential-inputs.md) for Cloudflare files and [macOS](references/macos.md) only for integration failures. Return credential handles and verification, never values.

[Official CLI guidance](https://developer.1password.com/docs/cli/secrets-environment-variables/)
