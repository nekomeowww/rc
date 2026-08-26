# rc

`rc` runs persistent, Kubernetes-backed development workspaces for coding agents.
It keeps repositories, Git worktrees, home directories, credentials, and agent processes as explicit Kubernetes resources, while `rcctl` provides the day-to-day command-line workflow.

The project consists of three programs:

- `rcctl` creates resources, imports credentials, and starts or reconnects to agent processes.
- `rc-kube` supervises processes inside Workspace and Environment Pods.
- `rc-controller` reconciles rc resources in Kubernetes.

> rc is under active development. Its APIs are currently `v1alpha1` and may
> change between releases.

## Why worktree-native?

rc models a remote Git repository and a writable checkout separately:

```text
Git remote -> Repository parent PVC -> Worktree child PVC -> Workspace
                                                        \-> AgentProcess
```

A `Repository` is the synchronized, authoritative mirror of a Git remote. A `Worktree` is an independent CSI clone of that Repository volume initialized with native `git worktree add` semantics. A `Workspace` mounts one or more Worktrees together with a persistent home directory, and can run multiple concurrent `AgentProcess` resources.

This gives each task an ordinary Git branch and working tree without repeatedly downloading the same remote. Worktrees remain inspectable after a process exits, and a disconnected terminal does not stop the process it started.

Because Worktrees and prepared Workspace Environments use Kubernetes PVC cloning, the selected StorageClass must support volume cloning. The default `local-path` StorageClass in a standard Kind cluster does not provide that capability. For local development on macOS or Linux, rc uses the SIG Storage `csi-driver-host-path` with a single-node Kind cluster. It is a sample/CI driver, not a production storage system, and implements cloning as a full file copy.

## Install

### Deploy the operator

Install the latest released controller, CRDs, and RBAC into the current Kubernetes context:

```sh
kubectl apply -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
kubectl rollout status deployment/rc-controller-manager -n rc-system
```

The released manifest uses matching versions of the public controller and runner images:

- `ghcr.io/nekomeowww/rc/controller`
- `ghcr.io/nekomeowww/rc/runner`

Your production StorageClass must support CSI PVC cloning. The local Kind setup described below is intentionally disposable and must not be used for production data.

### Install rcctl

Download a binary archive for Linux, macOS, or Windows from [GitHub Releases](https://github.com/nekomeowww/rc/releases), or install it from source with Go:

```sh
go install github.com/nekomeowww/rc/cmd/rcctl@latest
```

`rcctl` uses the same kubeconfig resolution as `kubectl`. Every command accepts
`--kubeconfig`, `--context`, and `--namespace` (`-n`):

```sh
rcctl --context kind-rc-dev -n development repo list
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
| `rcctl worktree` | Create and list independent native Git worktrees |
| `rcctl env` | Prepare and commit reusable Workspace home environments |
| `rcctl workspace` | Create persistent development machines and manage their mounts |
| `rcctl agent` | Run, list, reconnect to, stop, and inspect persistent processes |

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

To make an existing Codex login available to an Agent Process, import the local credential file:

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
```

The command creates a child PVC through CSI cloning and initializes a native Git worktree on it. Advanced `git worktree add` modes are available through flags such as `--ref`, `--detach`, `--orphan`, `--no-checkout`, and `--lock`.

### Run a process

The shortest path is to let `rcctl` create a generated Workspace and a writable Worktree from an existing Repository:

```sh
rcctl -n development agent run --repo rc --image ghcr.io/nekomeowww/rc/runner:latest --storage-class csi-hostpath-sc --agent-credential codex --cwd /workspace/rc -- codex
```

For a named development machine, create the Workspace first and mount the Worktree explicitly:

```sh
rcctl -n development workspace create dev --image ghcr.io/nekomeowww/rc/runner:latest --storage-class csi-hostpath-sc
rcctl -n development workspace mount worktree rc-readme --workspace dev --path rc
rcctl -n development workspace default dev

rcctl -n development agent run --agent-credential codex --cwd /workspace/rc -- codex
```

`agent run` attaches an interactive terminal. `agent exec` runs a non-terminal command and returns its exit code. Agent Processes are persistent resources, so you can inspect and reconnect to them independently of the original terminal:

```sh
rcctl -n development agent list

# Replace PROCESS_ID with a value printed by `agent list`:
# rcctl -n development agent logs PROCESS_ID
# rcctl -n development agent resume PROCESS_ID
```

Use `rcctl --help` and `rcctl <command> --help` for the complete command surface.

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

## License

Licensed under the
[Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0).
