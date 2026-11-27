# Installation

This guide covers cluster prerequisites, deploying the `rc` operator, installing `rcctl` and Agent Skills, setting up a disposable local Kind cluster, deploying development builds, and uninstalling.

## Cluster prerequisites

`rc` uses Kubernetes CSI PVC cloning for `Repository` → `Worktree` checkouts and `WorkspaceEnvironment` → `Workspace` clones.

- **CSI volume cloning**: The selected `StorageClass` must support PVC cloning (`dataSource` / `dataSourceRef` of kind `PersistentVolumeClaim`).
- **Immediate binding**: Workspace home storage must use a `StorageClass` with `volumeBindingMode: Immediate`. `rc` creates the runtime Pod only after the home PVC is bound; a `WaitForFirstConsumer` class reports `StorageReady=False` (`WaitForFirstConsumerUnsupported`).
- **Linux Worktree hot-mount (default)**: Adding Worktrees to a running Linux Workspace without restarting the Pod uses a privileged mount helper and `hostPath` bind mounts.

> [!NOTE]
> The default `local-path` StorageClass in a standard Kind cluster does not support PVC cloning and uses `WaitForFirstConsumer`. Use `make setup-kind` below for local development.

## Deploy the operator

Install the latest released controller, CRDs, and RBAC into the current Kubernetes context:

```sh
kubectl apply -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
kubectl rollout status deployment/rc-controller-manager -n rc-system
```

The install manifest deploys `rc-controller-manager` into the `rc-system` namespace and configures the controller, the injected `rc-kube` image, and the default Linux runner image.

### Published container images

Each stable release publishes matching versions of these public images:

| Image | Platforms | Role |
| --- | --- | --- |
| `ghcr.io/nekomeowww/rc/controller` | `linux/amd64`, `linux/arm64` | Kubernetes controller manager and Data Plane Gateway binary |
| `ghcr.io/nekomeowww/rc/rc-kube` | `linux/amd64`, `linux/arm64` | Injected Linux process supervisor and `tini` |
| `ghcr.io/nekomeowww/rc/runner` | `linux/amd64`, `linux/arm64` | Default Linux Workspace runner |
| `ghcr.io/nekomeowww/rc/runner-wayland` | `linux/amd64` | Headless Wayland GUI + Computer Use Linux runner |
| `ghcr.io/nekomeowww/rc/runner-windows` | `windows/amd64` | Experimental Windows Workspace runner (select via `spec.image` or `--windows-runner-image`) |

Releases also publish derived Codex CLI images under the same namespace (`openai-codex-cli` for Linux `amd64`/`arm64` and `openai-codex-cli-windows` for Windows `amd64`) tagged with `rc-<rc-version>`, `<codex-version>-rc.<rc-version>`, and `latest`.

## Install `rcctl`

### Homebrew (macOS and Linux)

```sh
brew install --cask nekomeowww/rc/rcctl
```

Homebrew adds the `nekomeowww/rc` tap automatically when using the fully qualified cask name. Each stable `rc` release updates the cask.

### Release binaries or Go

Download signed archives for macOS, Linux, or Windows from [GitHub Releases](https://github.com/nekomeowww/rc/releases), or install from source with Go:

```sh
go install github.com/nekomeowww/rc/cmd/rcctl@latest
```

### Kubeconfig resolution

`rcctl` resolves kubeconfig the same way as `kubectl`. Every command accepts `--kubeconfig`, `--context`, and `--namespace` (`-n`):

```sh
rcctl --context kind-rc-dev -n development repo list
```

## Install Agent Skills

`rc` ships reusable skills for coding agents, installed through the [Skills CLI](https://www.skills.sh/docs). The CLI prompts you to select the skills that fit your workflow:

```sh
pnpm dlx skills add nekomeowww/rc
```

With `npx`:

```sh
npx skills add nekomeowww/rc
```

Keep account mappings, private endpoints, and credential caches on the access device outside shared skills. See [local access and migration](../../skills/use-rc/references/local-access.md).

## Try locally with Kind

For a disposable local cluster on macOS or Linux, install Docker and [Kind](https://kind.sigs.k8s.io/), then create a single-node cluster with the SIG Storage `csi-driver-host-path` driver:

```sh
git clone https://github.com/nekomeowww/rc.git
cd rc
make setup-kind KIND_CLUSTER=rc-dev
kubectl config use-context kind-rc-dev
kubectl create namespace development --dry-run=client -o yaml | kubectl apply -f -
```

`make setup-kind` installs a pinned `csi-driver-host-path` release and creates the `csi-hostpath-sc` StorageClass. Volumes live inside the Kind node, cloning performs a full file copy, and all data is lost when the cluster or driver Pod is recreated. Do not use it for production data.

## Deploy a development build

To build and deploy images from a local checkout into Kind:

```sh
make docker-build IMG=rc-controller:dev RC_KUBE_IMG=rc-kube:dev
make docker-build-runner RUNNER_IMG=rc-runner:dev
kind load docker-image rc-controller:dev rc-kube:dev rc-runner:dev --name rc-dev
make deploy IMG=rc-controller:dev RC_KUBE_IMG=rc-kube:dev RUNNER_IMG=rc-runner:dev
```

`make docker-build` builds both the controller image and the `rc-kube` image that it injects. Deploy matching versions together.

To publish images to a registry reachable by your cluster:

```sh
# make docker-build docker-push IMG=REGISTRY/rc/controller:TAG RC_KUBE_IMG=REGISTRY/rc/rc-kube:TAG
# make docker-build-runner docker-push-runner RUNNER_IMG=REGISTRY/rc/runner:TAG
```

To generate a single distributable manifest:

```sh
make build-installer IMG=rc-controller:dev RC_KUBE_IMG=rc-kube:dev RUNNER_IMG=rc-runner:dev
kubectl apply -f dist/install.yaml
```

## Uninstall

Delete `rc` custom resources first if their persistent volumes are no longer needed, then remove the operator and CRDs:

```sh
kubectl delete -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
```

> [!WARNING]
> Removing the CRDs deletes all `rc` custom resources in the cluster. Review your `StorageClass` reclaim policy before uninstalling a production deployment.
