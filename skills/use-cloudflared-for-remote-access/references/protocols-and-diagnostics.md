# Protocols and diagnostics

Determine where TLS terminates: HTTP reverse proxy, opaque TCP, and SOCKS are different. `access tcp` forwards bytes; it does not itself create a SOCKS service.

For Kubernetes client-certificate auth, raw origin `tcp://<api-host>:<port>` plus a local TCP listener preserves API TLS. An HTTP TLS-terminating proxy generally cannot forward the original client certificate; the edge certificate is not Kubernetes authentication. Other Kubernetes auth mechanisms may differ. For config edits read [kubeconfig](kubeconfig.md).

With a genuine remote SOCKS upstream, preserve the API server and use a local SOCKS `proxy-url`; do not send direct API TLS to that port. Remote DNS may work when local resolution fails. Verify streaming separately from normal API reads.

| Symptom | Check |
| --- | --- |
| Refused | Device, bind, listener ownership/lifetime |
| EOF/reset | TCP/SOCKS/HTTP mismatch, upstream, TLS |
| Redirect/Access rejection | Target app, identity, policy, expiry |
| WebSocket bad handshake | Sanitized status/redirect, Access policy, WebSocket handling and ingress/connector logs; not one predetermined cause |
| CA/name error | API CA and TLS identity; keep verification enabled |
| Anonymous / Forbidden | Kubernetes credentials/TLS termination / operation-specific RBAC |

Use authorized control-plane inspection or ask for sanitized configuration when evidence is missing; do not change routes/policies to bypass diagnosis.

```sh
kubectl --kubeconfig <config> --context <context> --request-timeout=15s get --raw=/version
kubectl --kubeconfig <config> --context <context> --request-timeout=15s auth whoami
rcctl --kubeconfig <config> --context <context> -n <namespace> workspace list
```

If whoami is unsupported, use an allowed identity check; anonymous version access proves no identity.

[Cloudflare kubectl guide](https://developers.cloudflare.com/cloudflare-one/tutorials/kubectl/)
