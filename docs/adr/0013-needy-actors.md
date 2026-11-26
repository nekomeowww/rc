---
status: accepted
---

# ADR 0013: needy Actors

[needy](https://github.com/needy-dev/needy) runs a driving program, such as a
coding agent, in an actor: a Firecracker VM with no network interface. It
routes the program's commands to executors on other machines. A daemon
schedules actors and executors on runners, which dial the daemon. rc places
these parts on Kubernetes. needy keeps routing, the gateway, traces, and the
profile schema.

## Decision

### One daemon per namespace

The manager runs one needy daemon in each namespace that has an Actor or an
executor Workspace. Its Deployment uses the `Recreate` strategy, because the
state is SQLite on a ReadWriteOnce volume. It scales to zero when neither
remains, and the volume stays. An ended Actor keeps the daemon until its
runner's credential is revoked and for ten minutes more, so that its run's
logs and report can be read; without a state volume they are lost when the
daemon stops. A daemon is never shared between namespaces:
needy has no tenant model, so the namespace is the tenant boundary.

The daemon runs with `--disable-host-files`, because a `host:` file mapping
would read or write the daemon's own Pod. Its gateway refuses the ranges of
`--needy-gateway-block-cidrs`. A NetworkPolicy does not stop the daemon from
dialing its own loopback, and needy checks each resolved address.

The daemon's readiness probe runs inside its container, through the
management socket, so it does not depend on the network path from the
node.

The manager needs an admin client of each daemon to mint runner credentials
and create runs. It generates a static client token before the daemon
starts, keeps it in a Secret that only the manager reads, and gives the
daemon only its hash, through the daemon's static clients file. The runners
file and the clients file share one Secret, mounted as a directory so the
daemon reloads both when they change.

### The Actor resource

An Actor is one needy run. Its runner Pod runs `needy runner serve --roles
actor --ephemeral` on a KVM node and dials the daemon. The Pod mounts no home
volume, runs no rc-kube, and has no Service Account token. Neither the VM nor
the Pod listens. The VM reaches the network only through the daemon's
gateway.

The profile is needy's own YAML: a builtin name, a ConfigMap key, or inline
text. rc sets only the runner of the actor and of each executor; needy
validates the rest. A part without a runner would run on the daemon's own
Pod, so rc refuses a profile that names runners, leaves an executor unbound,
or sets `actor.network.services`, whose URLs bypass the gateway's policy.
After the runner connects, rc asks needy to check the placement and refuses
any finding.

The reconciler mints a credential per Actor, with a lifetime of
`maxRunDuration` plus a start grace. It starts the Pod, waits for the runner
to offer Firecracker, and creates the run. The status comes from needy's run
record, because an ephemeral runner exits 0 whatever the actor's outcome.
The credential is revoked only after the Pod has ended; a runner whose
credential is refused exits with an error. `maxRunDuration` and
`idleTimeout`, measured by needy's own activity record, stop the run.
Deleting an Actor stops its run first.

The spec is immutable. A new run is a new Actor.

### Executor Workspaces

`spec.needy.executor` turns a Linux Workspace into a needy executor. The
manager generates its runner credential into a Secret that the Workspace
owns, and gives the daemon only its hash, through the daemon's static runners
file. An init container copies needy from the needy image, as for rc-kube in
[ADR 0010](0010-inject-rc-kube-from-the-controller.md). A resident service of
[ADR 0009](0009-resident-workspace-services.md) runs the runner. The runner
reports exec traffic with `rc-kube process touch`, so a Workspace that runs
an Actor's commands is not suspended as idle.

### Network

The actor's gateway runs in its actor runner, so the actor's traffic
leaves from the actor Pod and the daemon carries only control traffic.

With `--needy-network-policies` the manager keeps two NetworkPolicies in
each namespace. They are optional because they need a CNI that enforces
them, and because a cluster may have its own:

- Actor Pods reach the internet except `--needy-gateway-block-cidrs` and the
  metadata address, the daemon's control port, and cluster DNS. Nothing
  reaches them. The runner refuses the same ranges for the addresses an
  actor names; the policy also covers the Pod itself.
- The daemon admits the namespace's actor runners and Workspaces, and the
  manager's Pods, to its control port.

`spec.proxy` sends an Actor's traffic through a SOCKS5 proxy of any
implementation. The runner refuses a connection when the proxy fails
instead of dialing directly. The credentials come from a Secret that is
mounted readable only by the Pod's group. With `socks5h` the proxy resolves
names, so it must refuse private, cluster, and metadata addresses itself.
With the manager's policies, `podSelector` admits the actor Pod to a proxy
inside the cluster through a policy for that Actor alone; without them, rc
adds no policy, because one for the proxy alone would cut the Pod off from
the daemon. The actor Pod keeps its internet egress: the runner downloads
the base VM image when it starts.

### Configuration

needy is off until the manager has `--needy-image`. A KVM pool is described by
its extended resource, node selector, tolerations, and the group that owns
`/dev/kvm`; these differ between clusters. The needy image, the daemon, the
runners, and rc's generated client must come from one needy release, because
needy's API has no compatibility promise before 1.0.

## Consequences

- The Workspace field must not be set before the manager that knows it runs:
  an older manager updates Workspaces with its own types and drops the field.
- A profile whose image layers are not built yet needs a daemon that can
  build them. Until needy can build them elsewhere, `--needy-daemon-kvm`
  places the daemon on a KVM node.
- needy's listeners have no TLS. Tokens and credentials cross the Pod network
  in the clear; a NetworkPolicy or a mesh must protect them. The
  NetworkPolicies that rc can keep for needy are a separate, optional
  integration.
- Executors on users' own machines need an ingress that carries gRPC to the
  namespace's daemon. They are not part of this decision.

## Rejected options

- **An Actor as a Workspace.** A Workspace has a home volume, rc-kube as the
  first process, hot-mounts, and LXCFS. LXCFS hides the CPU features that
  Firecracker checks, and rc-kube would supervise needy, which supervises the
  VM.
- **rc's own profile schema.** It would follow every needy release.
- **One daemon for the cluster.** needy authorizes runners and clients, not
  tenants.
- **Status from the Pod phase.** The ephemeral runner's exit code does not
  reflect the actor.
