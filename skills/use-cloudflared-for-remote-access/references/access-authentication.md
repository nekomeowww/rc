# Access authentication

## Login

```sh
cloudflared access tcp --hostname <endpoint> --url 127.0.0.1:<port>
```

Reuse supported login state or the installed `access login` flow. If a consumer request triggers approval, promptly give the exact emitted URL; keep that flow observable and stop competing probes until the user completes it. Then retry once (the original request may have timed out). Do not fabricate callbacks, approve for the user, or extract cookies. Retry expired flows deliberately; stop repeated policy/configuration failures.

## Service Token

An Access ST's Client ID/Secret authorizes the target Access app when its policy accepts it, typically via Service Auth. It is neither a connector token, management API token, nor Kubernetes/1Password Service Account. Keep these identities distinct.

Check `access tcp --help` for `TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET`; inject only into the consumer environment through [credential inputs](credential-inputs.md), not secret-valued flags or logs. HTTP applications may instead accept `CF-Access-Client-Id`/`CF-Access-Client-Secret` headers through a secure client input. Non-HTTP clients cannot be assumed to add HTTP headers.

On rejection, inspect the selected app, policy, expiry and credential source; do not silently change identities or add Bypass. Verify origin identity/permissions separately.

[Service Tokens](https://developers.cloudflare.com/cloudflare-one/access-controls/service-credentials/service-tokens/) · [CLI inputs](https://github.com/cloudflare/cloudflared/blob/master/cmd/cloudflared/access/cmd.go)
