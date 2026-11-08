---
name: use-cloudflared-for-remote-access
description: Connect through Cloudflare Access or manage requested Tunnel publishing. Covers login, Service Tokens, TCP/SOCKS routing, and handshake diagnostics.
---

# Use cloudflared for remote access

Identify device, endpoint, upstream protocol, authentication, direction, and lifetime; reuse working access and check installed help. Connecting remotely does not authorize publishing locally. Access auth does not replace origin auth.

Read only relevant references:

- [Authentication](references/access-authentication.md): login or Service Token.
- [Credential inputs](references/credential-inputs.md): op references, `.env`, or saved launcher.
- [Kubeconfig](references/kubeconfig.md): create/update Kubernetes contexts.
- [Protocols/diagnostics](references/protocols-and-diagnostics.md): TCP, SOCKS, TLS, failures.
- [Publishing](references/publishing.md): create/change a Tunnel or share a service.

Use [1Password](../use-op-for-secret-access/SKILL.md) only for needed op reads. Reuse prior authorization; silence does not grant new plaintext caching, policy changes, or public exposure. Inspect authorized management tools when configuration is unclear; otherwise ask for sanitized details.

Bind listeners to loopback, reuse verified matching listeners, and never kill unrelated port owners. Keep login observable. Use owned sessions/process groups or an authorized supervisor; stop temporary wrappers and children on every exit and verify ports close. Retain pre-existing or explicitly persistent services.

Return route, verified consumer operation, credential handles, lifetime/stop method, and blockers—not secret values or merely “listener started.”
