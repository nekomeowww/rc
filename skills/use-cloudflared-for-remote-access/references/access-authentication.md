# Cloudflare Access authentication

Access protects a target application. Its credentials do not replace Kubernetes,
SSH, database, or other origin credentials. Select a mode accepted by the target
Access policy, not merely one for which a credential happens to exist.

## Browser login

Use the installed `cloudflared access login` flow when appropriate, or start the
client listener and issue one bounded consumer request to trigger login:

```sh
cloudflared access tcp --hostname <access-hostname> --url 127.0.0.1:<port>
```

Keep the listener observable. Once it reports a pending login, stop consumer
probes that could initiate competing authorization flows. Wait for the user to
complete the emitted flow, then retry the consumer request once. A blocked request
may already have timed out while the user approved; that is not evidence that a
new login or listener is necessary.

Keep the listener observable. Give the exact emitted authentication URL to the
user promptly, without fabricating a callback or logging cookies/JWTs. Monitor the
same pending flow and validate it with sequential bounded requests. Do not spawn
concurrent logins or restart a pending login just because the user has not answered.
Retry an expired flow deliberately; stop repeated configuration/policy failures.
Never automate approval or extract browser credentials. Reuse supported login state
until it expires. If a browser is launched, preserve its login profile and close
the owned browser when finished.

## Access Service Token

Distinguish these identities before reading any secret:

| Credential | Purpose |
| --- | --- |
| Access Service Token (Client ID + Client Secret) | Machine access to an Access application accepting it |
| Tunnel connector token | Runs a connector; does not authorize client Access requests |
| Cloudflare management API token | Reads/changes permitted account configuration |
| Origin Service Account or credential | Authenticates to the underlying application |

“Service Account” is provider-specific; ask which system owns it. A 1Password
Service Account and Kubernetes ServiceAccount are not Cloudflare Access tokens.

Confirm the target policy accepts the chosen token (typically a Service Auth
policy), and inspect expiry/revocation or token rotation when rejected. Do not add
Bypass or broaden policies to make a probe work. Management permission to create a
token is separate from authorization to access the origin.

For TCP forwarding, inspect `cloudflared access tcp --help` for supported
`TUNNEL_SERVICE_TOKEN_ID` and `TUNNEL_SERVICE_TOKEN_SECRET` environment variables.
Inject them into only that process using the approved secret manager or private
cache. Do not place resolved values in flags, shell history, logs, or service unit
text. If these inputs are unsupported in the installed release, use its documented
authentication method or an authorized update rather than guessing flags.

For an HTTP Access application, documented requests can use `CF-Access-Client-Id`
and `CF-Access-Client-Secret` headers. Supply them through the HTTP client's secure
input mechanism, not literal argv values. Do not assume arbitrary non-HTTP clients
can add these headers; use the appropriate client-side tunnel or private route.

Verify the target app through the final client path. Access success does not prove
origin identity or RBAC. Keep a rejected token's identity/scope intact while
investigating; do not silently switch accounts or fall back to a human login.

Sources:

- [Arbitrary TCP with cloudflared](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/cloudflared-authentication/arbitrary-tcp/)
- [cloudflared Access CLI source](https://github.com/cloudflare/cloudflared/blob/master/cmd/cloudflared/access/cmd.go)
- [Access Service Tokens](https://developers.cloudflare.com/cloudflare-one/access-controls/service-credentials/service-tokens/)
