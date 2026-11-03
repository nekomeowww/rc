---
status: accepted
---

# ADR 0009: Resident Workspace services

A Workspace sold as a managed device runs a resident agent: a long-lived
process that connects out to a gateway and serves tool calls. A WorkspaceExec
does not fit. It runs at most once and becomes Lost when the runtime is
replaced, and while it runs it blocks idle suspension and deletion forever.

## Decision

`spec.services` lists resident processes. Each has a `name`, an exact
`command`, an optional `workingDirectory`, and an optional `env`.

- The `rc-kube serve` supervisor starts every service on each runtime start,
  including after resume, and restarts it with a capped exponential delay
  whenever it exits. Shutdown stops services with the usual stop grace.
- A service is never activity. Starting or restarting one does not touch the
  activity record of
  [ADR 0008](0008-runtime-activity-and-maximum-run.md), and services are not
  executions, so they never block idle suspension, maximum-run suspension,
  topology changes, or deletion. Real work reports itself with
  `rc-kube process touch`; services receive `RC_KUBE_SOCKET` for this.
- A service receives the Workspace `env`, which a WorkspaceExec also receives by
  default, overridden by its own `env`. Credentials stay opt-in per
  WorkspaceExec and are not given to services.
- The list reaches the supervisor as one encoded `rc-kube serve` argument, like
  lifecycle actions. A value from a Secret or ConfigMap becomes a runtime
  container variable that Kubernetes resolves at container start; the
  supervisor resolves and removes it before it starts any process. The list
  is part of the runtime topology, so a change replaces the runtime Pod only
  once no execution is active. Without services, the hash is unchanged.
- Each run is an ordinary supervisor process record with a `service_` ID, so
  existing transcript, list, and log commands show it. The supervisor keeps
  a few finished runs per service.
- Linux and Windows runtimes support services. The Darwin runtime starts the
  supervisor from a provider hook without container variables, so the API
  rejects services there.

## Rejected options

- **A WorkspaceExec with a "resident" flag.** Every lifecycle, admission, and
  retention path treats a nonterminal execution as active, and an execution is
  at most once by design. A flag would add an exception to each of them.
- **Sidecar containers.** A sidecar does not share the supervisor's process
  tree, transcripts, or stop handling, and on Windows and Darwin the runtime
  is one container. It would also restart independently of the runtime.
- **A ConfigMap read by the supervisor.** It needs ownership and cleanup, and
  either updates live, which contradicts "on the next runtime start", or needs
  the same Pod hash as an argument.
- **Resolving Secret values in the controller.** The values would then be
  stored in the Pod spec.
