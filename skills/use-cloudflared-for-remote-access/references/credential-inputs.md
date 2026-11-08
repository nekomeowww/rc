# Credential input modes

Select the mode the user requested. These modes all feed the same Access listener;
the local kubeconfig still supplies Kubernetes credentials. Read
[kubeconfig adaptation](kubeconfig.md) for that separate configuration.

| Supplied input / intent | Action and retained state |
| --- | --- |
| Endpoint + op references, one-time | Resolve for one consumer process via explicit account; retain no resolved values or permanent kubeconfig |
| Endpoint + op references, cache as `.env` | Respect the explicit caching authorization, write a private data file, then use the file path without further op reads until authorized refresh |
| Endpoint + op references, reusable script | Store account/references/procedure only; resolve at launch and reuse the running listener; no promise of bypassing 1Password approval |
| Endpoint + existing `.env` | Validate and parse the supplied file as data, inject selected variables, do not invoke op or rewrite the file |
| Endpoint + browser login | Use Access login/approval; do not request a Service Token just because another mode supports it |

For op modes invoke [secret access](../../use-op-for-secret-access/SKILL.md) with
the exact account/references and persistence scope. For file-only and login modes,
skip that dependency entirely. An endpoint is not enough to infer the origin
protocol, Kubernetes CA, TLS name, or Kubernetes user.

## Dotenv is data, not a program

A file may contain plaintext values or `op://` references. Inspect the selected
keys' type privately; do not pass unresolved references as Access credentials.
Reference-only files require the selected op account and approved resolution;
plaintext files do not require 1Password. Do not guess between mixed modes.

For plaintext Access ST input, accept only the required mappings
`TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET` unless the user identifies
an existing schema that needs explicit mapping. Parse through a non-executing
dotenv reader or a small local parser with a defined grammar:

- Permit blank lines, comments, and one literal `KEY=value` assignment per line.
  Handle quoted values only if the parser explicitly supports their escaping rules.
- Reject duplicate required keys, missing/empty values, malformed assignments,
  unsupported syntax, and unapproved keys before launching the consumer. Do not
  partially use a valid first key with an old second key inherited from the shell.
- Never `source`, shell-`eval`, interpolate command substitutions, or treat the file
  as a shell script. `$()`, backticks, and variable references must never execute;
  a narrow parser may reject them. Never print values in parse errors.
- Open only the selected user-owned regular file with owner-only permissions (or
  equivalent Windows ACL), reject symlinks and unsafe parent-directory ownership,
  and handle missing/replaced files without falling back to another credential.
  Validate the opened descriptor with `fstat` and read that same descriptor; a
  path-only `lstat` followed by reopening leaves a replacement race. Use no-follow
  and bounded/nonblocking open where supported so a FIFO cannot hang the loader.
  Check parent-directory ownership/write permissions and use descriptor-relative
  traversal when stronger path-replacement protection is needed; do not claim
  race-free traversal from checking only the final path.
- Start only the intended child with the two values in its environment; keep them
  out of CLI args, logs, and rc remote environment pass-through. An environment is
  visible to processes with sufficient local privileges.

## Persist a newly resolved ST

A request to persist is already authorization within the stated target/path scope;
do not ask again merely because this reference is loaded. Explain plaintext-cache
risk if not yet discussed. Recommend
`${XDG_CONFIG_HOME:-$HOME/.config}/rc/secrets/<profile>.env`, including on macOS.
Keep names private and follow the caller's approved location. Before writing,
check effective Git global ignores and any applicable dotfiles/project tracking;
ignore rules do not cover already tracked files or other sync tools. Follow the
[secret-access persistence procedure](../../use-op-for-secret-access/SKILL.md).

Create the file atomically with mode `0600` inside a private `0700` directory.
Use a serialization that round-trips through the chosen parser, including values
containing `=`, spaces, or quotes; reject unsupported values without truncating or
corrupting an existing cache. A failed second secret read must not leave a partial
cache. Never overwrite unrelated content or refresh rejected/rotated credentials
by silently choosing another account. Retain only the explicitly requested cache;
remove temporary files and listeners after use.

## Saved launchers

Keep endpoint, explicit account, references, bind, and local config handles in a
private script rather than resolved secret values. `op run` is usually one unlock
boundary for a listener lifetime; a script capturing `op read` output must equally
avoid secret argv/output and repeated unnecessary reads. Do not use `eval` on
secret output. A launcher cannot guarantee provider approval will never recur.
Distinguish reusing a running listener, agent authorization, provider session
expiry, and authorization to store credentials for unattended use.

The launcher must own and stop its consumer process tree, including on parse/read
failure, pending login cancellation, and interrupt. A saved launcher is not an
instruction to create a boot service or leave a process running indefinitely.
