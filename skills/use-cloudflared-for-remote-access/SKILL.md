---
name: use-cloudflared-for-remote-access
description: Connect through Cloudflare Access, establish or inspect Cloudflare Tunnel routes, or publish an explicitly requested local service. Use for cloudflared TCP/SOCKS access, login or Service Token authentication, quick tunnels, and handshake diagnostics.
---

# Use cloudflared for remote access

Determine the direction and protocol before changing a route. Connecting to an
existing remote endpoint and publishing a local service have different effects.
Cloudflare Access authentication is separate from the origin application's auth.

## Inspect and choose

1. Establish the executing device, target, connection direction, protocol, local
   bind, authentication mode, and needed lifetime. Reuse an existing verified route
   and supervisor. A local port number does not establish its protocol.
2. Check `cloudflared --version` and relevant `access tcp`, `access login`, and
   `tunnel` help. Inspect existing configuration and logs without exposing tokens,
   cookies, private hostnames in published artifacts, or credential-bearing URLs.
3. Select only the needed reference:

   | Need | Read |
   | --- | --- |
   | Connect to an existing Access app with browser login or Service Token | [Access authentication](references/access-authentication.md) |
   | Reference injection, cached `.env`, provided `.env`, or a reusable launcher | [Credential input modes](references/credential-inputs.md) |
   | Create or update a Kubernetes context for the selected route | [Kubeconfig adaptation](references/kubeconfig.md) |
   | Raw TCP, SOCKS, Kubernetes TLS, or connection/handshake failure | [Protocols and diagnostics](references/protocols-and-diagnostics.md) |
   | Create/change a Tunnel route or publish a local service | [Publishing and Tunnel management](references/publishing.md) |

4. If the route or policy is unknown, inspect the authorized Cloudflare management
   tool/API if available. Check its installed help and read only the relevant
   account's application, policy, ingress/route, and connector state. `cloudflared`
   and Wrangler do not automatically provide every Zero Trust administration API.
   Ask for missing configuration when access is unavailable; do not infer a
   specific cause from `WebSocket: bad handshake` alone.
5. Read credentials from the user's approved source. If that source is 1Password,
   invoke `$use-op-for-secret-access`
   ([entrypoint](../use-op-for-secret-access/SKILL.md)) with the account, exact
   references, consumer, and authorization scope. Skip it for a working login or
   already approved cache. Do not make 1Password a prerequisite for Cloudflare.

## Authorization and lifetime

Respect explicit and prior authorization; do not ask again for the same approved
connection. In unattended use, reuse an approved machine identity/cache and route.
If the user does not answer an optional preference, continue with existing
protected access or read-only preparation. Silence does not authorize publishing
an unprotected local service, changing Access policies, or caching plaintext secrets.

Bind client-side listeners to loopback by default. Before starting one, check
whether the port is occupied and whether a verified matching listener can be
reused. Never stop an unrelated process to free the port.

Use an observable foreground session for temporary access or the device's existing
service manager for authorized persistent access. Linux systemd, macOS launchd,
and Windows services are alternatives, not prerequisites. Auto-start installation
requires scope to persist access; keep secrets out of unit/launch command arguments.
Record process/session/service ownership, logs, expiry, and stop method. Stop
agent-owned temporary listeners by their exact PID/session when finished, including
error paths; supervise the whole owned process tree when using wrappers such as
`op run`, and confirm that the listener closes. Leave explicitly retained or
pre-existing services intact.

## Handoff

Return the connection direction, route kind, listener or endpoint, authentication
mode, origin TLS expectations, verified consumer operation, lifetime, owner and
stop method. Return credential handles rather than values. A listening socket or
healthy connector alone does not prove Access policy, upstream protocol, origin
identity, or application authorization. Let the caller verify its actual operation.
