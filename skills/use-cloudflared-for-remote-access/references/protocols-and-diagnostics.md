# Protocol selection and diagnostics

Identify every hop and where TLS terminates. An HTTP/HTTPS origin route is a
reverse proxy; an opaque TCP route forwards bytes; a SOCKS proxy negotiates a
separate protocol. `cloudflared access tcp` does not itself implement SOCKS.

For creating or updating a selected Kubernetes context, read
[kubeconfig adaptation](kubeconfig.md) before writing configuration.

## Raw Kubernetes API TCP

For Kubernetes client-certificate authentication, preserve the TLS connection
between the client and API server. A compatible published Tunnel origin uses
`tcp://<api-host>:<api-port>`, with an Access TCP listener on the client device:

```sh
cloudflared access tcp --hostname <access-hostname> --url 127.0.0.1:<local-port>
```

The selected kubeconfig can use `server: https://127.0.0.1:<local-port>` and the
correct `tls-server-name` for the API certificate. Preserve CA trust and Kubernetes
client authentication. In this direct TCP shape, do not add a SOCKS `proxy-url`.
Use a separate test kubeconfig when adaptation is needed; preserve relative paths
to CA/key/certificate files if relocating a config. Do not change global context
or overwrite unrelated entries during diagnosis.

Cloudflare's edge certificate authenticates the edge hostname; it does not make
the API server see the original TLS client certificate after HTTP TLS termination.
If an existing HTTPS reverse proxy cannot preserve required client-certificate
authentication, propose an authorized raw TCP or private-network route. Not every
Kubernetes setup uses mTLS: bearer tokens and exec plugins can have different
requirements. Determine the actual auth mechanism before changing a route.

## Remote SOCKS upstream

If the Tunnel origin actually provides SOCKS, keep the API endpoint and pair the
TCP listener with the kubeconfig's proxy setting:

```yaml
server: https://<api-host>:<api-port>
proxy-url: socks5://127.0.0.1:<local-port>
```

Here the TCP listener forwards SOCKS negotiation to the remote SOCKS server.
Sending TLS directly to this listener as though it were the API can produce EOF.
Conversely, sending SOCKS negotiation to a raw API listener is wrong. Preserve
remote DNS behavior; local inability to resolve an API hostname does not prove
that a remote proxy cannot reach it. Verify streaming exec/attach operations when
needed: success on an ordinary resource read does not establish client/proxy
support for upgrades or all streaming protocols.

## Verify in layers

Use the same selected config/context and bounded requests. For Kubernetes:

```sh
kubectl --kubeconfig <config> --context <context> --request-timeout=15s get --raw=/version
kubectl --kubeconfig <config> --context <context> --request-timeout=15s auth whoami
rcctl --kubeconfig <config> --context <context> -n <namespace> workspace list
```

If whoami is unsupported, use an allowed identity/authorization check. A public
version response does not prove authentication. Never dump kubeconfig or disable
TLS verification to make a check pass.

| Evidence | Inspect next |
| --- | --- |
| Connection refused | Executing device, bind address, listener owner/lifetime |
| EOF/reset | Raw TCP versus SOCKS versus HTTP, upstream connectivity, then TLS |
| Login redirect or Access rejection | App hostname, identity, accepted auth mode, policy and token expiry |
| WebSocket bad handshake | Sanitized status/redirect, Access result, app/policy match, WebSocket handling, route and connector logs |
| CA or hostname mismatch | Intended API identity, CA chain and TLS server name |
| Kubernetes anonymous identity | Client credentials and TLS termination |
| Expected identity but Forbidden | RBAC for the specific operation |

A bad handshake is a symptom, not proof of one cause. Inspect available client
and authorized control-plane evidence; if missing, ask for sanitized ingress,
application/policy, and relevant logs. Do not request full tokens or kubeconfig.
Keep an existing server route intact until the failure and proposed change are
understood and authorized. A long-lived private-network/Cloudflare One Client
route may be more suitable than client-side TCP forwarding; select based on the
supported deployment and workload, not as an automatic migration.

Sources:

- [Cloudflare kubectl guide](https://developers.cloudflare.com/cloudflare-one/tutorials/kubectl/)
- [Tunnel protocols](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/protocols/)
- [Kubernetes kubeconfig API](https://kubernetes.io/docs/reference/config-api/kubeconfig.v1/)
