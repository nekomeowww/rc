# rc

`rc` runs persistent, Kubernetes-backed development workspaces for coding agents.
It keeps repositories, Git worktrees, home directories, credentials, and processes as explicit Kubernetes resources, while `rcctl` provides the day-to-day command-line workflow.

The project consists of three programs:

- `rcctl` creates resources, imports credentials, and starts or reconnects to processes.
- `rc-kube` supervises processes inside Workspace and Environment Pods.
- `rc-controller` reconciles rc resources in Kubernetes.

> rc is under active development. Its APIs are currently `v1alpha1` and may
> change between releases.

## Why worktree-native?

rc models a remote Git repository and a writable checkout separately:

```text
Git remote -> Repository parent PVC -> Worktree child PVC -> Workspace
                                                        \-> WorkspaceExec
```

A `Repository` is the synchronized, authoritative mirror of a Git remote. A `Worktree` is an independent CSI clone of that Repository volume initialized with native `git worktree add` semantics. A `Workspace` mounts one or more Worktrees together with a persistent home directory, and can run multiple concurrent `WorkspaceExec` resources.

This gives each task an ordinary Git branch and working tree without repeatedly downloading the same remote. Worktrees remain inspectable after a process exits, and a disconnected terminal does not stop the process it started.

Because Worktrees and prepared Workspace Environments use Kubernetes PVC cloning, the selected StorageClass must support volume cloning. The default `local-path` StorageClass in a standard Kind cluster does not provide that capability. For local development on macOS or Linux, rc uses the SIG Storage `csi-driver-host-path` with a single-node Kind cluster. It is a sample/CI driver, not a production storage system, and implements cloning as a full file copy.

## Install

### Deploy the operator

Install the latest released controller, CRDs, and RBAC into the current Kubernetes context:

```sh
kubectl apply -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
kubectl rollout status deployment/rc-controller-manager -n rc-system
```

Each release publishes matching versions of these public images:

- `ghcr.io/nekomeowww/rc/controller`
- `ghcr.io/nekomeowww/rc/runner`
- `ghcr.io/nekomeowww/rc/runner-wayland`
- `ghcr.io/nekomeowww/rc/runner-windows`

The install manifest configures the controller and Linux runner. Select the
experimental Windows runner explicitly on a Windows Workspace or through the
manager's `--windows-runner-image` flag.

When `rc-kube serve` starts as Linux PID 1, it starts itself under Tini. Tini
reaps orphaned descendants while rc-kube keeps ownership of direct command
exit statuses. The Linux runner includes `tini`; custom images that run
`rc-kube serve` as PID 1 must provide it on `PATH`. Existing init systems and
Windows/macOS runtimes do not use this wrapper.

Run `bash hack/test-runner-reaping.sh <runner-image>` to check orphan cleanup,
command exit status, and supervisor shutdown in an isolated Docker container.

Releases also publish `openai-codex-cli` (Linux amd64/arm64) and
`openai-codex-cli-windows` (Windows amd64) under the same GHCR namespace.
These derived images use `rc-<rc-version>` and `<codex-version>-rc.<rc-version>`
tags, plus `latest`; the four runtime images use `<rc-version>` tags.
For example, release `v0.12.1` with Codex `0.154.0` publishes
`openai-codex-cli:rc-0.12.1` and `openai-codex-cli:0.154.0-rc.0.12.1`.
Existing tags containing only the Codex version remain available but are no
longer updated by this workflow.

Push a stable `vMAJOR.MINOR.PATCH` tag to start the complete release. GitHub
Release assets are staged as a draft, and the release becomes public only after
all six images and their attestations succeed. Linux Codex extends the exact
runner version from that release. A failed release stays a draft; fix the
failure and rerun the failed jobs before treating that version as complete.
Image tags can become visible while the draft is building.
The separate Codex workflow can still be dispatched against a selected ref
with a released runner version and an optional Codex version override.

Your production StorageClass must support CSI PVC cloning. The local Kind setup described below is intentionally disposable and must not be used for production data.

### Install rcctl

On macOS or Linux, install the latest release from the Homebrew tap:

```sh
brew install --cask nekomeowww/rc/rcctl
```

Homebrew adds `nekomeowww/rc` automatically when the fully qualified cask name
is used. Each stable rc release updates the cask.

Alternatively, download a binary archive for Linux, macOS, or Windows from
[GitHub Releases](https://github.com/nekomeowww/rc/releases), or install it from
source with Go:

```sh
go install github.com/nekomeowww/rc/cmd/rcctl@latest
```

`rcctl` uses the same kubeconfig resolution as `kubectl`. Every command accepts
`--kubeconfig`, `--context`, and `--namespace` (`-n`):

```sh
rcctl --context kind-rc-dev -n development repo list
```

### Install agent skills

Install rc's agent skills with the [Skills CLI](https://www.skills.sh/docs).
The command prompts for the target agent and scope; select only the skills that
fit the intended workflow:

```sh
pnpm dlx skills add nekomeowww/rc --skill use-rc --skill setup-rc --skill setup-rc-locally --skill setup-lobehub-cli
```

`npx` is equivalent when pnpm is unavailable:

```sh
npx skills add nekomeowww/rc --skill use-rc --skill setup-rc --skill setup-rc-locally --skill setup-lobehub-cli
```

## Quick start with rcctl

For a disposable local cluster on macOS or Linux, install Kind and Docker, then create a single-node cluster with the clone-capable CSI hostpath driver:

```sh
git clone https://github.com/nekomeowww/rc.git
cd rc
make setup-kind KIND_CLUSTER=rc-dev
kubectl config use-context kind-rc-dev
```

`setup-kind` pins and installs the upstream `csi-driver-host-path` release and creates the `csi-hostpath-sc` StorageClass. Driver volumes live inside the Kind node and are lost when the cluster or driver Pod is recreated.

The examples below use the `development` namespace and the local StorageClass. When using another cluster, replace `csi-hostpath-sc` with one whose CSI driver supports PVC cloning.

```sh
kubectl create namespace development --dry-run=client -o yaml | kubectl apply -f -
```

The top-level command groups follow the rc resource model:

| Command | Purpose |
| --- | --- |
| `rcctl credentials` | Import Git, agent, and process credentials |
| `rcctl repo` | Clone, inspect, execute commands in, and delete Repository mirrors |
| `rcctl worktree` | Create, inspect, execute in, and delete independent native Git worktrees |
| `rcctl env` | Prepare and commit reusable Workspace home environments |
| `rcctl workspace` | Create persistent development machines and manage their mounts |
| `rcctl run` / `rcctl exec` | Run commands in a new / existing Workspace |
| `rcctl ps` / `attach` / `logs` / `stop` / `inspect` / `rm` | List and manage processes |

### Import credentials

For a public repository, no Git credential is required. For GitHub repositories, `rcctl` can read the token from an authenticated GitHub CLI session and store it as a namespaced rc `Credential` backed by a Kubernetes Secret:

```sh
gh auth login
rcctl -n development credentials import --type github
```

The default resource name is `github-com`. Pass it when cloning a private repository. Placeholder commands are comments so copying the block does not run them accidentally:

```sh
# rcctl -n development repo clone https://github.com/OWNER/REPOSITORY.git \
#   --name repository \
#   --storage-class csi-hostpath-sc \
#   --credential-ref github-com
```

To make an existing Codex login available to an process, import the local credential file:

```sh
rcctl -n development credentials import --type agent --agent codex --file "$HOME/.codex/auth.json"
```

This creates an `AgentCredential` named `codex`. Credential resources and their Secrets stay in the selected namespace. A generic credential file can also be projected into explicitly selected processes:

```sh
# rcctl -n development credentials import \
#   --type process \
#   --name tool-auth \
#   --file ./credentials.json \
#   --mount-path /home/agent/.tool/credentials.json \
#   --env TOOL_HOME=/home/agent/.tool
```

### Clone a repository

Create the persistent Repository mirror and wait for its initial synchronization:

```sh
rcctl -n development repo clone https://github.com/nekomeowww/rc.git --name rc --storage-class csi-hostpath-sc
```

Inspect it or run an exact command against the parent volume:

```sh
rcctl -n development repo list
rcctl -n development repo exec rc -- git status --short
```

Repository parents are not writable Workspace checkouts. Create a Worktree when you want an isolated branch:

```sh
rcctl -n development worktree add --repo rc --name rc-readme --branch docs/readme
rcctl -n development worktree list
rcctl -n development worktree exec rc-readme -- git status --short
```

The add command creates a child PVC through CSI cloning and initializes a native Git worktree on it. `worktree exec` is the lightweight path for short commands that need only the base Runner Image: it runs in a separate Job, does not allocate a Workspace home PVC, and holds the same exclusive write Lease as a Workspace mount. It intentionally does not provide Workspace Environment state, caches, credentials, process persistence, or an interactive terminal. Use `run --rm --worktree rc-readme -- COMMAND` when a command needs those Workspace capabilities. Advanced `git worktree add` modes are available through flags such as `--ref`, `--detach`, `--orphan`, `--no-checkout`, and `--lock`. Delete an unmounted Worktree and its owned PVC and bootstrap Job with `rcctl worktree rm rc-readme`.

### Run a process

The shortest isolated path is to explicitly request a temporary Workspace and a writable Worktree from an existing Repository:

```sh
rcctl -n development run -it --rm --repo rc --image ghcr.io/nekomeowww/rc/runner:latest --storage-class csi-hostpath-sc --agent-credential codex --cwd /workspace/rc -- codex
```

`run` always creates a new Workspace. It retains that Workspace by default;
`--rm` requests cleanup after all its processes terminate, with a five-minute
grace period for reading results and logs. Generated Worktrees from `--repo`
are owned by the new Workspace. Existing Worktrees selected with `--worktree`
are never deleted by this cleanup. Use `--name` to choose the new Workspace name.

For a named development machine, create the Workspace first and mount the Worktree explicitly:

```sh
rcctl -n development workspace create dev --image ghcr.io/nekomeowww/rc/runner:latest --storage-class csi-hostpath-sc --agent-credential codex
rcctl -n development workspace mount worktree rc-readme --workspace dev --path rc
rcctl -n development workspace default dev

rcctl -n development exec -it --agent-credential codex --cwd /workspace/rc dev -- codex
```

#### Optimizations

##### npm

rc natively supports selecting an npm registry for each Workspace. Point
`--npm-registry` at an npm-compatible registry, cache, or proxy to speed up
dependency downloads while keeping the configuration scoped to that Workspace:

```sh
rcctl workspace create rc-dev \
  --npm-registry http://verdaccio.rc-system.svc.cluster.local:4873
```

For example, you can deploy Verdaccio as a caching proxy in the cluster. A
ClusterIP Service named `verdaccio` in the `rc-system` namespace is reachable
from other namespaces at
`http://verdaccio.rc-system.svc.cluster.local:4873`.

The convenience flag sets two `Workspace.Spec.Env` defaults:
`NPM_CONFIG_REGISTRY` receives the URL with one trailing slash, while Corepack
receives the same URL without a trailing slash through its independent
`COREPACK_NPM_REGISTRY` variable.

The same flag is available on `run` and `exec`. With an existing
Workspace, it overrides registry defaults only for that WorkspaceExec and does
not modify the Workspace. With `run`, the generated Workspace receives
the defaults. Supplying either registry variable explicitly through `--env` or
`--env-file` conflicts with `--npm-registry`. This convenience flag configures
registry locations only; it does not provide registry tokens, usernames, or
passwords. Configure authentication separately when the selected registry
requires it.

Both `run` and `exec WORKSPACE` wait for completion and return the command's
exit code by default. Use `-it` for an interactive terminal or `-d` to detach.
Each command creates a persistent `WorkspaceExec`, so you can inspect and
reconnect independently of the original terminal. Put rcctl flags before the
Workspace name for `exec`; everything after that name is command argv.

```sh
rcctl -n development ps                    # Running processes in this namespace
rcctl -n development ps -a                 # Include pending and completed processes
rcctl -n development ps --workspace dev    # Filter by Workspace

# Replace PROCESS_ID with a value printed by `ps`:
# rcctl -n development inspect PROCESS_ID
# rcctl -n development logs PROCESS_ID
# rcctl -n development attach PROCESS_ID
```

List commands use compact, width-aware tables and shorten long values with an
ellipsis. Pass `-o wide` for secondary columns, or `-o json`/`-o yaml` for the
complete resource data. `inspect`, `workspace get`, `repo get`, and
`worktree get` show an untruncated detail view of one resource.

Use `rcctl --help` and `rcctl <command> --help` for the complete command surface.

The execution API is now `workspaces.rc.ayaka.io/v1alpha1`, kind
`WorkspaceExec`. This replaces `AgentProcess`; the old `rcctl agent` command
group has been removed. Existing process records are not automatically migrated.
Before upgrading an existing installation, use the old CLI/controller to stop
and remove old process records after saving any needed logs. Upgrade the CRDs,
controller, and CLI together. Do not relabel completed executions as new
WorkspaceExec resources: recreating them requests a new command execution.

## Deploy a development build

To deploy images built from the checkout into the local Kind cluster, build and load both images with matching development tags:

```sh
make docker-build IMG=rc-controller:dev
make docker-build-runner RUNNER_IMG=rc-runner:dev
kind load docker-image rc-controller:dev rc-runner:dev --name rc-dev
make deploy IMG=rc-controller:dev RUNNER_IMG=rc-runner:dev
```

To publish the images, replace the commented registry names with repositories that the target cluster can pull:

```sh
# make docker-build docker-push IMG=REGISTRY/rc/controller:TAG
# make docker-build-runner docker-push-runner RUNNER_IMG=REGISTRY/rc/runner:TAG
```

To generate one distributable manifest for the locally loaded images, run:

```sh
make build-installer IMG=rc-controller:dev RUNNER_IMG=rc-runner:dev
kubectl apply -f dist/install.yaml
```

## Uninstall

Delete rc custom resources before removing the CRDs if their persistent data is no longer needed. Then remove the released installation:

```sh
kubectl delete -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
```

Removing the CRDs deletes all rc custom resources from the cluster. Review the associated PVC retention behavior before uninstalling a production deployment.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the Kind development loop, tests, generated-file rules, and pull request checklist.

Design decisions and the detailed runtime model live under [`docs/`](docs/).

Experimental platform guides:

- [Attach a native Windows 11 Kubernetes worker](docs/guides/experimental-windows-11-worker.md)
- [Run Windows Workspaces](docs/guides/windows-workspaces.md)
- [Run Darwin Workspaces with macOS-vz-kubelet](docs/guides/darwin-workspaces.md)

## License

Licensed under the
[Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0).
