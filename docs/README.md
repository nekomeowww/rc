# rc Documentation

Start with the section that matches how you use `rc`.

## By audience

### Developers & CLI users

Use `rcctl` to work with Git mirrors, isolated Worktrees, persistent Workspaces, and supervised processes.

- [Installation](guides/installation.md) — install the operator, `rcctl`, Agent Skills, or a disposable local Kind cluster.
- [Using rcctl](guides/rcctl.md) — credentials, repositories, worktrees, workspaces, `run` / `exec`, process management, lifetimes, and npm caching.
- [GUI applications](guides/run-gui-applications.md) — run headless Wayland GUI applications and Computer Use workflows on Linux.
- [Doctor](doctor.md) — read-only diagnostics for storage, runtime health, references, and deletion blockers.

### Platform engineers & SDK users

Build an agent sandbox platform on top of Kubernetes using `rc`'s CRDs and opt-in Workspace Data Plane.

- [Workspace Data Plane (`execd` & `envd`)](guides/data-plane.md) — enable `spec.dataPlane`, deploy the mTLS Data Plane Gateway, and connect with `@alibaba-group/opensandbox` (`execd`), `e2b` (`envd`), or plain HTTP/Connect clients.
- [Workspace runtime & lifetimes (ADR 0007)](adr/0007-workspace-lifetimes.md) — retention policies, idle suspension, and maximum run duration.
- [Resident workspace services (ADR 0009)](adr/0009-resident-workspace-services.md) — run long-lived daemons inside a Workspace without blocking idle suspension.
- [Domain glossary](../CONTEXT.md) — canonical terminology for Repositories, Worktrees, Credentials, Workspaces, and the Data Plane.

### Cluster operators & Kubernetes developers

Deploy, secure, upgrade, and operate `rc` across Linux, Windows, and macOS worker nodes.

- [Installation & cluster prerequisites](guides/installation.md) — CSI PVC cloning requirements, published container images, release manifests, and uninstall.
- [Operations & custom images](guides/operations.md) — image upgrades, `rc-kube` / `tini` injection, custom Linux images, scheduling & Pod metadata, and API migrations.
- [LXCFS for Linux Workspaces](guides/lxcfs.md) — expose container-scoped CPU, memory, uptime, and load metrics inside Linux Workspaces.
- [Windows Workspaces](guides/windows-workspaces.md) — run `spec.os: windows` Workspaces with ConPTY and Job Objects.
- [Experimental Windows 11 worker node](guides/experimental-windows-11-worker.md) — attach a Windows 11 worker to a Kubernetes cluster.
- [macOS Workspaces](guides/darwin-workspaces.md) — run `spec.os: darwin` Workspaces on `macOS-vz-kubelet` VM Pods.

### Contributors & architecture

- [Contributing guide](../CONTRIBUTING.md) — Kind development loop, unit/integration/e2e test suites, code generation, and pull request checklist.
- [Architecture Decision Records (`docs/adr/`)](adr/) — why `rc` models storage, runtimes, lifetimes, image injection, and the data plane the way it does.
- [Design specifications (`docs/design/`)](design/) — detailed specifications for repository sync, worktree storage planning, execution history, runtime recovery, and Data Plane compatibility.

## Where to put what

| Document type | Location | Purpose |
| --- | --- | --- |
| Front door & overview | [`README.md`](../README.md) | Orientation, architecture at a glance, quick start, and links into `docs/` |
| User & operator guides | `docs/guides/` | Task-focused setup, workflow, SDK, and operations guides |
| Diagnostics reference | [`docs/doctor.md`](doctor.md) | `rcctl doctor` checks and status mapping |
| Shared vocabulary | [`CONTEXT.md`](../CONTEXT.md) | Canonical domain terms and terms to avoid |
| Architecture decisions | `docs/adr/` | Accepted and superseded design decisions with rationale |
| Design specifications | `docs/design/` | Detailed component contracts and behavior specs |
| Agent skills | [`skills/`](../skills/) | Installable agent skills for `rcctl`, cluster setup, and remote access |
