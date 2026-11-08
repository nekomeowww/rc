# Publishing

Only read for requested server-side Tunnel changes or local sharing. Existing client access does not require creating connectors or DNS records.

Inspect local versus remotely managed configuration, ingress order/fallback, origin protocol/TLS, existing consumers, and the separate Access app/policy. Use authorized management tools for remote configuration; editing local YAML does not update it. Keep origin verification enabled and choose protocol/client requirements deliberately. Named Tunnels and DNS alone do not restrict access.

For explicitly authorized temporary public sharing:

```sh
cloudflared tunnel --url http://127.0.0.1:<port>
```

Confirm the service, exposed data/routes, audience, protection, and lifetime. If public exposure was not authorized, ask before starting; silence is not approval. A random URL is not access control. Do not accidentally publish APIs, credentials or admin/debug surfaces. Verify any protected-sharing mode against installed/current documentation.

Quick Tunnels are for testing/development; check current limits/config conflicts. Verify allowed and unauthorized behavior where protection is required, record ownership/stop method, and stop temporary tunnels on every exit. Preserve unrelated configuration/services and retain rollback for shared-route changes.

[Quick Tunnels](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/) · [Tunnel documentation](https://developers.cloudflare.com/tunnel/)
