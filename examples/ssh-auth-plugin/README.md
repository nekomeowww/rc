# External gRPC SSH Auth Plugin Example

This standalone Go module demonstrates how an external control plane (for example a multi-DC SaaS platform or an enterprise identity adapter) implements `rc.datad.ssh.v1alpha1.SSHAuthService` so the `rc` Data Plane Gateway can authenticate SSH clients and map `(authority, username, public_key)` to a local-cluster `Workspace`.

## 1. Enable Go Workspace (`go.work`)

From the repository root, initialize a local `go.work` file so this example resolves `github.com/nekomeowww/rc/apis/sdk/go/datad/ssh/v1alpha1` directly from the local checkout without editing `go.mod`:

```sh
go work init . ./examples/ssh-auth-plugin
```

## 2. Run the Example Plugin

```sh
go run ./examples/ssh-auth-plugin \
  --listen 127.0.0.1:9090 \
  --username alice \
  --authority ws-1.example.com \
  --namespace team-a \
  --workspace sandbox-1 \
  --pubkey-files ~/.ssh/id_ed25519.pub
```

## 3. Point the `rc` Data Plane Gateway at the Plugin

```sh
/manager data-plane-gateway \
  --listen :8080 \
  --ssh-tls-listen :2223 \
  --ssh-tls-cert-file /etc/rc-ssh-tls/tls.crt \
  --ssh-tls-key-file /etc/rc-ssh-tls/tls.key \
  --ssh-host-key-file /etc/rc-ssh/ssh_host_ed25519_key \
  --ssh-auth-grpc-target 127.0.0.1:9090 \
  --ssh-auth-grpc-timeout 3s
```

## 4. Connect with TLS SNI (`~/.ssh/config`)

```sshconfig
Host *.example.com
  ProxyCommand openssl s_client -quiet -connect %h:443 -servername %h -alpn ssh 2>/dev/null
```

Then connect using a plain username and per-Workspace subdomain:

```sh
ssh alice@ws-1.example.com
```
