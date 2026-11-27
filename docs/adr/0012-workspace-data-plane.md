---
status: accepted
---

# ADR 0012: Workspace Data Plane

An orchestrator that runs in an edge or serverless runtime, such as a
Cloudflare Worker, can manage Workspaces through the Kubernetes API. But it
cannot read or write files in a Workspace or stream a process without
Kubernetes Pod exec or `rcctl`. Until this decision, `rc-kube serve` listened
only on a Pod-local Unix socket.

## Decision

A Workspace can enable a Workspace Data Plane with `spec.dataPlane`. The wire
contract is E2B's runtime `envd` (`envd.yaml`, `filesystem.proto`,
`process.proto`), pinned to one upstream commit. A Data Plane Gateway is the
single entry point for external callers.

### envd runs inside rc-kube

`rc-kube serve` implements `envd`. rc does not ship the upstream `envd` binary
or a sidecar. A Data Plane Process is then an ordinary supervisor process
record, so it has a transcript and is in the process list. It also follows the
stop grace and the activity record of
[ADR 0008](0008-runtime-activity-and-maximum-run.md). The injected `rc-kube`
of [ADR 0010](0010-inject-rc-kube-from-the-controller.md) brings the Data Plane
to every Linux image without a change to the image.

A Data Plane Process is activity while it runs. It is not an execution. It
does not hold back the maximum run, runtime replacement, or deletion, and it is
lost when the runtime is replaced. A Kubernetes record per process would remove
the reason for a lightweight data plane.

### The runtime listens on TCP only for the gateway

On Linux and Windows, `rc-kube serve` listens on port `49983` of the Pod IP. The
listener accepts only TLS connections with a client certificate of the gateway
(mTLS). The runtime does not know the caller's token. The server certificate of
each Workspace is in a Secret volume, so it can rotate without a new Pod. Its
name, `<name>.<namespace>.workspace.rc.ayaka.io`, does not depend on the Pod
IP. By default the manager owns the CA. A flag can select a cert-manager Issuer
instead.

A Darwin runtime is a VM behind NAT, so the gateway cannot reach its Pod IP.
There, the gateway uses Pod exec to start a bridge into `rc-kube`. Pod exec is
already authorized by Kubernetes RBAC.

### The gateway checks the caller

Each Workspace has one access token, as in E2B. It comes from a
`SecretKeyRef`, or the controller generates one into a Secret that the
Workspace owns. The gateway compares the caller's `X-Access-Token` with the
Secret in constant time and also checks E2B signed file URLs. A rotated Secret
takes effect at the gateway at once. The Pod spec never contains the token or a
value derived from it. A token that is missing or shorter than 32 bytes blocks
the Workspace.

The gateway is its own Deployment, built from the manager image. It never
starts a suspended Workspace.

## Rejected options

- **The upstream `envd` binary as a sidecar or a second daemon.** Its processes
  would have no rc transcript and no stop handling, and they would not report
  activity. A Windows or Darwin runtime is one container or one VM.
- **The token, or its SHA-256, in the Pod.** The runtime could then check
  callers without the gateway. But a value in the Pod spec is readable by
  everyone who can read Pods, and a rotated token stays stale until the Pod is
  replaced.
- **Only a NetworkPolicy in front of the port.** It protects nothing when the
  CNI does not enforce it.
- **Pod exec on every platform.** It needs no listener, but all file data then
  goes through the API server and the kubelet.
- **Kubernetes RBAC for each caller request.** It needs no token. But a Worker
  must then keep a long-lived Kubernetes credential, and the E2B signed URLs
  cannot work.
