---
name: use-distribution-as-oci-registry
description: Create, deploy, verify, and troubleshoot a reproducible private OCI registry using CNCF Distribution and Docker Compose, optionally behind an Nginx TLS-terminating proxy. Use when Codex needs to produce a working registry deployment bundle, launch it, validate `/v2/`, test authenticated push and pull, inspect Buildx manifests, or diagnose proxy-related 502 and unauthorized upload failures.
---

# Use Distribution as an OCI Registry

Produce a runnable deployment rather than only explaining individual settings. Prefer Docker Compose unless the user specifies another orchestrator.

## Deploy

1. Read [references/deployment.md](references/deployment.md) before creating deployment files.
2. Determine the requested output directory, public registry hostname, certificate source, storage path, and deployment host from user context. Never copy hostnames, addresses, ports, usernames, or paths from examples or prior environments.
3. Create a self-contained deployment bundle containing pinned container versions, persistent storage, basic authentication, TLS, restart policies, and secret exclusions. Parameterize environment-specific values.
4. Keep Distribution reachable only through the proxy network unless the user explicitly needs a separately bound diagnostic port.
5. Generate secrets securely and keep passwords out of command-line literals, committed files, logs, and responses.
6. Run `docker compose config` before starting the stack. Then run `docker compose up -d` and inspect service state and logs.

## Verify

Require all of these checks before declaring success:

1. An unauthenticated `GET https://<registry>/v2/` returns `401`, `Docker-Distribution-Api-Version: registry/2.0`, and `WWW-Authenticate`.
2. `docker login <registry>` succeeds.
3. A disposable image can be tagged, pushed, removed locally, and pulled back.
4. A requested multi-platform image exposes every expected platform through `docker buildx imagetools inspect`.

Treat login-only verification as incomplete because blob uploads exercise redirects and authentication differently.

## Diagnose

- For `SSL_do_handshake() ... wrong version number`, probe the upstream with HTTP and HTTPS. Match `proxy_pass` to the actual upstream protocol. Remove `proxy_ssl_verify` for an HTTP upstream.
- When login succeeds but push returns `unauthorized`, inspect the blob-upload `Location`. Require the external HTTPS scheme and hostname. Set `X-Forwarded-Proto $scheme` on a TLS-terminating proxy.
- Keep `proxy_redirect off` if the proxy should preserve Distribution-generated upload locations; do not use it as a replacement for forwarded headers.
- For `413`, raise or disable the proxy body-size limit. For stalled uploads, disable request buffering and increase proxy timeouts.
- For certificate failures, serve a complete trusted chain or install the private CA on every client and builder. Do not weaken production clients with insecure-registry settings.

## Change Safely

Back up existing production configuration, preserve unrelated settings, validate Nginx before reload, and repeat the complete push/pull verification after every proxy change.
