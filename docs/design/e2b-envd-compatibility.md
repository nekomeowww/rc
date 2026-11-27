# Spec: Workspace Data Plane (E2B `envd` wire compatibility)

Status: Ready for agent implementation

Decision record: [ADR 0012](../adr/0012-workspace-data-plane.md).

## Problem Statement

External orchestrators in edge or serverless runtimes, such as Cloudflare
Workers, can manage `Workspace` and `WorkspaceExec` resources through the
Kubernetes REST API. But they have no lightweight, Web-standard way to read and
write files in a running Workspace, watch directories, or stream process
output. Today they must speak the Kubernetes Pod exec SPDY/WebSocket protocols
or run `rcctl`.

Every Linux Workspace runtime Pod already runs the injected `rc-kube`
supervisor ([ADR 0010](../adr/0010-inject-rc-kube-from-the-controller.md)).
Its control plane is reachable only through a Pod-local Unix socket, which
Kubernetes Pod exec bridges. Workflows that seed playbooks or MCP
configurations, or that fetch generated screenshots, need brittle shell
workarounds.

## Solution

A Workspace can enable a **Workspace Data Plane**. `rc-kube serve` implements
the E2B runtime `envd` wire contract: the `envd.yaml` REST endpoints,
`filesystem.proto`, and `process.proto`. A **Data Plane Gateway** is the single
cluster entry point. It authenticates each request with the Workspace's access
token and routes it to the Workspace that the request names.

rc promises wire compatibility only. A caller uses plain `fetch`, the Connect
clients generated from the protos, or the E2B SDK with an explicit envd URL
(`sandboxUrl` together with debug mode). rc does not implement the E2B
control-plane API, so `Sandbox.create()` and `Sandbox.connect()` against
`api.<domain>` are not supported.

## User Stories

1. As a workflow orchestrator on Cloudflare Workers, I want to reach a running
   Workspace with `fetch`, or with the E2B SDK pointed at the gateway URL, so
   that I can manage files and run commands without Pod exec or `rcctl`.
2. As an agent orchestrator, I want to upload a file with `POST /files`, so
   that I can inject playbooks, temporary MCP configurations, or task inputs
   before I start an agent.
3. As an agent orchestrator, I want `POST /files` to create missing parent
   directories, so that I need no separate round trip to create them.
4. As an agent orchestrator, I want to upload several files in one multipart
   request (as `sandbox.files.write` with a list does), so that seeding skills
   or configuration files is fast.
5. As an agent orchestrator, I want to download a file with `GET /files` as a
   binary stream, so that I can fetch screenshots, build artifacts, or logs
   without large payloads in etcd.
6. As an agent orchestrator, I want to give a browser or another service a
   signed `/files` URL (`signature`, `signature_expiration`), so that it can
   download or upload one path without the access token.
7. As an agent orchestrator, I want `Stat`, `ListDir`, `MakeDir`, `Move`, and
   `Remove` through `filesystem.Filesystem`, so that the E2B SDK filesystem
   methods work.
8. As an agent orchestrator, I want to watch a directory (`WatchDir`,
   `CreateWatcher`, `GetWatcherEvents`, `RemoveWatcher`), so that I can react to
   artifacts or status files that a running agent writes.
9. As an agent orchestrator, I want to start and stream commands through
   `process.Process` (`Start`, `Connect`, `List`, `SendInput`, `CloseStdin`,
   `SendSignal`), so that `sandbox.commands.run` and `sandbox.commands.start`
   work.
10. As an agent orchestrator, I want PTY processes with resize (`Update`), so
    that interactive E2B terminal sessions work.
11. As a Workspace operator, I want every Data Plane Process to run under the
    `rc-kube` supervisor, so that it is in the process list, writes a standard
    transcript, and follows the stop grace.
12. As a Workspace operator, I want Data Plane requests and running Data Plane
    Processes to count as activity
    ([ADR 0008](../adr/0008-runtime-activity-and-maximum-run.md)), so that a
    Workspace is not idle-suspended while a caller uses it.
13. As a Workspace operator, I want a Data Plane Process not to hold back the
    maximum run, runtime replacement, or deletion, so that a forgotten process
    cannot keep compute forever.
14. As a Workspace operator, I want the Data Plane to be opt-in per Workspace,
    so that a Workspace without it keeps its current Pod, security posture, and
    topology hash.
15. As a Workspace operator, I want every request except `GET /health` to need
    the Workspace access token, so that nobody can reach files or processes
    anonymously.
16. As a Workspace operator, I want the controller to generate the access token
    unless I reference my own Secret, so that the default is secure and an
    orchestrator can still mint its own token.
17. As a Workspace operator, I want Data Plane operations to run as the
    Workspace runtime user, so that file ownership matches `WorkspaceExec` and
    Workspace Mounts.
18. As a Workspace operator, I want no change to Runner Images or Workspace
    Environment images, so that every Linux Workspace gets the Data Plane
    through the injected `rc-kube`.
19. As a cluster administrator, I want one gateway that maps
    `/namespaces/<namespace>/workspaces/<name>/` to the Workspace runtime, so
    that one Ingress or Cloudflare Tunnel route reaches every Workspace without
    per-Workspace Ingresses or wildcard DNS.
20. As a cluster administrator, I want the gateway to return `503` for a
    suspended Workspace and `404` for a missing Workspace or one without a Data
    Plane, so that a caller can tell these apart from an authentication error.
21. As a cluster administrator, I want only the gateway to be able to talk to a
    runtime's Data Plane port, so that other Pods in the cluster cannot bypass
    the token check.
22. As a Workspace operator, I want `WorkspaceExec`, `rcctl exec`, `rcctl run`,
    and resident `spec.services`
    ([ADR 0009](../adr/0009-resident-workspace-services.md)) to work the same
    with or without the Data Plane.

## Implementation Decisions

### Wire contract

- Pin the upstream spec at one recorded `e2b-dev/infra` commit (envd `0.11.0`;
  the research for this spec read `30a39cf3364197c162576f3823bb7e927f6c82d5`).
  Advertise that version in `X-Envd-Version`. The E2B SDK in debug mode
  assumes every feature, so implement the full contract of the pinned version,
  not a subset.
- REST:
  - `GET /health` returns `204` without authentication.
  - `GET /files` streams the file and supports `Accept-Encoding: gzip`.
  - `POST /files` accepts:
    - `multipart/form-data`, where each `file` part's filename is its target
      path, or the `path` query parameter is given; and
    - `application/octet-stream`, optionally with `Content-Encoding: gzip`.
  - It creates parent directories, overwrites existing files, supports
    `X-Metadata-*`, and returns `EntryInfo[]`.
  - Error codes follow upstream: `400`, `401`, `404`, `406`, `409`, and
    `507`.
  - `POST /init` is not implemented and returns `404`, because the controller
    supplies the token.
- ConnectRPC `filesystem.Filesystem`: `Stat`, `MakeDir`, `Move`, `ListDir`,
  `Remove`, `WatchDir` (with recursive watches and `entryInfo` in events),
  `CreateWatcher`, `GetWatcherEvents`, and `RemoveWatcher`.
- ConnectRPC `process.Process`: `List`, `Connect`, `Start`, `Update`,
  `SendInput`, `CloseStdin`, `SendSignal`, and `StreamInput`.
- Transport: Connect over HTTP/1.1 and HTTP/2, with JSON and binary codecs.
  `StreamInput` is the only client-streaming RPC and works only over HTTP/2.
  All other RPCs are unary or server-streaming, so `fetch` in `workerd` works.

### Users and paths

- Operations run as the Workspace runtime user. The E2B default user and the
  runtime user's name map to it. `root` and every other user are rejected with
  `PermissionDenied`. The user arrives as `Authorization: Basic <user>:` on RPCs
  and as `username` on `/files`.
- Path resolution follows upstream:
  - an empty path is the home directory;
  - `~` expands to the home directory;
  - a relative path is joined to the home directory; and
  - an absolute path is used as it is.
- rc does not confine paths. Operating-system permissions are the boundary, as
  for Pod exec and `rcctl cp`.

### Data Plane Processes and activity

- A process started with `process.Process.Start` is a Data Plane Process: a
  supervisor process record with an `envd_` ID prefix (like `service_` in ADR
  0009). It gets transcripts, listing, and stop grace from the supervisor. It
  receives the Workspace `env`, like a service, and no Credentials.
- Activity (ADR 0008):
  - every authenticated request touches the activity record when it starts and
    when it ends;
  - a running Data Plane Process holds activity, so the supervisor reports
    current activity while one runs; and
  - an open stream alone, such as `WatchDir` or `Connect` to a finished
    process, does not hold activity.
- A Data Plane Process is not an execution. It does not hold back maximum-run
  suspension, topology replacement, or `DeleteAfterProcessesExit` deletion. It
  is lost when the runtime is replaced. It has no Kubernetes record.

### Workspace API

- `spec.dataPlane` enables the Data Plane. An optional `accessTokenSecretRef`
  is a Secret Key Reference. Without it, the controller generates a random
  token of at least 32 bytes into a Secret that the Workspace owns, and names
  that Secret in `status.dataPlane`.
- If the referenced token is missing or shorter than 32 bytes, the Workspace is
  blocked. The controller reports the reason (for example
  `AccessTokenNotFound` or `InvalidAccessToken`) and does not create the
  runtime Pod.
- Without `spec.dataPlane`, the runtime Pod, its arguments, and its topology
  hash do not change. Enabling or disabling the Data Plane is a topology change
  and follows the normal replacement rules.
- The API rejects `spec.dataPlane` on Environment Draft editor Pods. Only
  Workspaces have it.
- Linux, Windows, and Darwin runtimes support the Data Plane.

### Runtime listener and mTLS (Linux and Windows)

- With the Data Plane enabled, `rc-kube serve` listens on `:49983` and accepts
  only TLS with a client certificate signed by the Data Plane CA. It does not
  receive the access token or any value derived from it.
- The server certificate of each Workspace has the name
  `<name>.<namespace>.workspace.rc.ayaka.io`, which does not depend on the Pod
  IP. It is mounted from a Workspace-owned Secret volume. `rc-kube` reloads it
  for each handshake, so rotation needs no new Pod.
- Certificates:
  - by default, the manager creates and rotates a CA Secret in its own
    namespace and issues the gateway client certificate and the Workspace
    server certificates; and
  - a manager flag can select a cert-manager Issuer instead, and then rc
    creates a `Certificate` for each Workspace.
- The Pod declares the container port. rc creates no NetworkPolicy, Service, or
  Ingress for it.

### Darwin bridge

- A Darwin runtime is a VM behind NAT. Its Pod IP cannot be reached from the
  cluster, and it has no container variables or Secret volumes.
- The gateway reaches a Darwin runtime through a long-lived Pod exec of an
  `rc-kube` bridge command. That command forwards HTTP/2 to the `envd` server
  inside `rc-kube serve`. Kubernetes RBAC for Pod exec authorizes this hop, so
  it needs no mTLS.

### Data Plane Gateway

- It is a separate Deployment built from the manager image: a new subcommand
  with its own ServiceAccount. It needs:
  - `get`, `list`, and `watch` on Workspaces, Pods, and the token Secrets;
  - `create` on `pods/exec` for Darwin; and
  - its client certificate.
- Routing: `https://<gateway>/namespaces/<namespace>/workspaces/<name>/` is the
  envd base URL. The gateway strips the prefix and forwards the rest.
- Authentication:
  - The gateway compares `X-Access-Token` with the Workspace's token Secret in
    constant time. It also verifies `/files` `signature` and
    `signature_expiration` with the same token.
  - A rotated Secret takes effect at once.
  - It returns `401` for a wrong or missing token.
- Status mapping:
  - `404`: the Workspace does not exist or has no Data Plane.
  - `503`: the Workspace is suspended or not Ready. The body names the state.
  - The gateway never starts a Workspace. A caller sets
    `desiredState: Running` through the Kubernetes API.
- Streaming: the gateway passes Connect streaming frames and chunked bodies
  through without buffering.
- TLS toward callers:
  - By default, the gateway serves HTTP/1.1 and h2c on a ClusterIP Service,
    and an operator puts an Ingress or a Cloudflare Tunnel in front.
  - `--tls-cert-file` and `--tls-key-file` make it terminate TLS itself.

## Testing Decisions

- **Philosophy**: test external behavior at the highest practical seam. Tests
  talk to the Data Plane like a real E2B client. They check filesystem state,
  exit codes, streamed output, HTTP and Connect status codes, and activity and
  lifecycle effects. They do not check supervisor maps or private helpers.
- **Seams, highest first**:
  1. **`rc-kube serve` Data Plane**:
     - Start `serve` with the listener on `127.0.0.1:0`, a test CA, and a
       temporary home.
     - Exercise `/health`, `/files` (multipart, octet-stream, gzip,
       metadata), every `filesystem.Filesystem` and `process.Process` RPC
       (stdout and stderr streaming, stdin, signals, PTY), and the user and
       path rules.
     - Check that a connection without a valid client certificate is refused,
       and check the ADR 0008 activity effects.
     - Optionally, run an E2B SDK smoke test in debug mode with `sandboxUrl`.
  2. **Gateway**:
     - Routing by path prefix to a real `rc-kube` listener over mTLS.
     - Token and signed-URL checks, including rotation.
     - Streaming passthrough.
     - The `401`, `404`, and `503` responses.
     - The Darwin exec bridge against a fake exec stream.
  3. **Controller (envtest)**:
     - Enabling the Data Plane adds the port, the certificate volume, and the
       listener argument.
     - A Workspace without it is unchanged, including its topology hash.
     - Token generation and the blocking conditions.
     - Certificate issue and rotation with the internal CA.
     - The editor rejection.
- **Platforms**: run the `rc-kube` seam on Linux, Windows, and Darwin, and the
  end-to-end path on a runtime of each platform.
- **Prior art**: the existing `rc-kube` server and supervisor tests (a real
  supervisor on a temporary directory with real child processes) and the
  controller reconciliation and admission tests.

## Out of Scope

- The E2B control-plane API (`/sandboxes`, `/v2/sandboxes/{id}/connect`,
  templates, billing, snapshots, `/freeze` and `/unfreeze`). So the unmodified
  `Sandbox.connect()` flow is not supported.
- Host-pattern routing (`49983-<id>.<domain>`), wildcard DNS, and
  per-Workspace Ingresses.
- `envd` `/init`, `/metrics`, and E2B traffic access tokens for other ports.
- Starting a suspended Workspace from the gateway.
- Running Data Plane operations as `root` or as another user.
- A Data Plane on Environment Draft editor Pods.
- Replacing `WorkspaceExec`, `rcctl exec`, or `rcctl attach`. The Unix socket
  and Pod exec paths stay.
- TensorChord's build-time `envd`.
- Credential isolation between processes in one Workspace (as in the Workspace
  runtime design).

## Further Notes

- The upstream `envd` specs are in `e2b-dev/infra` under
  `packages/envd/spec/` (`envd.yaml`, `filesystem/filesystem.proto`,
  `process/process.proto`) under the Apache-2.0 license.
- Downstream consumer: `@bam/executor` defines an E2B-like `Executor` and
  `Sandbox` abstraction. With the Data Plane and the gateway, it can attach
  `sandbox.files` and streaming `sandbox.commands` over `fetch` from Cloudflare
  Workers.
