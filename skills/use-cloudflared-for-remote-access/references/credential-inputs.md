# Credential inputs

| User input | Procedure |
| --- | --- |
| op references, temporary | Explicit account, one consumer-lifetime injection, no resolved cache |
| op references, persist `.env` | Respect caching authorization; atomically write a private cache, reuse until authorized refresh |
| op references, save launcher | Store procedure/references only; reuse listener, never promise provider approval disappears |
| Existing plaintext `.env` | Parse as data; no op call, file rewrite, or renewed scope question |
| Browser login | Follow [authentication](access-authentication.md); no ST required |

For op reads/persistence use [secret access](../../use-op-for-secret-access/SKILL.md). Plaintext files skip it; reference-only files need explicit op resolution, not passing `op://` strings to cloudflared. Resolve mixed formats explicitly.

## Dotenv

Parse only approved keys/mappings, normally `TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET`. Define supported literal/quoted syntax; never source, eval, or expand `$()`, backticks or variables. Reject malformed, duplicate, missing/empty or unsupported records before launching; never combine partial input with ambient credentials or print values in errors.

Require an owner-only regular file and safe parents; reject symlinks. Use descriptor-relative no-follow traversal where supported, nonblocking open, `fstat`, and read that same descriptor: path-only checks followed by reopening race, and FIFOs can hang. Fail safely on unsupported platforms; these checks do not isolate same-user/root attackers or in-place mutation.

Inject only selected child environment values, never argv/logs or remote rc pass-through. For new caches recommend `${XDG_CONFIG_HOME:-$HOME/.config}/rc/secrets/<profile>.env`, including macOS; follow approved locations and [persistence controls](../../use-op-for-secret-access/SKILL.md). Serialize values so the parser round-trips them; failed reads/serialization must leave no partial cache or overwrite. Preserve supplied files and unrelated content.

Launchers own their entire process tree and temporary files. A script containing `op read`/`op run` avoids repeated agent scope questions, not vault unlock/expiry; unattended credentials require separate authorization.
