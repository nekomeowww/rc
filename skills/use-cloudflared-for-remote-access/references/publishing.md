# Publishing and Tunnel management

Read this only for requested server-side Tunnel work or local-service publishing.
An existing `cloudflared access tcp` client connection does not require creating
a connector, DNS record, or new public hostname.

## Named or managed Tunnel

Inspect whether configuration is locally managed or remotely managed. For local
configuration, inspect ingress order, hostname, origin protocol, TLS settings,
and final fallback rule, then use installed validation/rule-testing commands.
For remotely managed configuration, use an authorized dashboard or management API
and inspect the relevant account/Tunnel only. Do not edit a local YAML file and
assume it changes a remotely managed route.

Confirm the exact origin and who may reach it before creating/changing a route.
Choose HTTP/HTTPS for an intended web reverse proxy; TCP/SSH/RDP and other supported
services have distinct client requirements. For end-to-end application mTLS or
SOCKS, read [protocol guidance](protocols-and-diagnostics.md). HTTPS origin
`originServerName`/CA settings authenticate the origin connection, not the user's
original client certificate. Keep origin TLS verification enabled.

Inspect Access application and policy separately from DNS and Tunnel health:
creating a named Tunnel or using a random hostname does not by itself restrict
users. Connector, management, and client credentials must retain separate scopes.
Check existing consumers before changing a shared route. Keep a rollback path for
the specific authorized configuration change.

## Quick Tunnel

For explicitly requested temporary sharing, a typical command is:

```sh
cloudflared tunnel --url http://127.0.0.1:<port>
```

This normally publishes a local service at a temporary `trycloudflare.com` URL.
Before starting, inspect the actual service, binding, exposed routes/data, and
application authentication. State the target and exposure and obtain confirmation
if public publishing was not already authorized. A random URL is not access control.
Do not expose Kubernetes APIs, credential endpoints, admin/debug consoles, or an
entire developer environment merely to make remote testing convenient.

For unattended work, use existing authorization describing the service, audience,
authentication, and lifetime. Without it, prepare the command or reuse an existing
protected path; do not infer public-sharing approval from silence. Where the
installed release provides a protected-sharing mode, verify its current documented
behavior and intended recipients before relying on it.

Quick Tunnels are for testing/development, not a persistent production route.
Check current limits and local config conflicts in the official docs rather than
promising uptime or protocol support. Verify from the intended client perspective
that allowed requests work and unauthorized requests are rejected when protection
is required. Record the exact owned process/session, URL, expiry/stop method, and
close temporary tunnels when the task ends, including failures. Do not alter a
user's pre-existing Tunnel configuration just to run a quick demo.

Sources:

- [Quick Tunnels](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/)
- [Tunnel configuration](https://developers.cloudflare.com/tunnel/)
