# Contributing to rc

Thanks for contributing to rc. This guide covers the local development loop,
Kind-based integration testing, code generation, and deployment of development
images.

## Prerequisites

- Go 1.26 or newer, matching `go.mod`
- Docker with BuildKit support
- `kubectl`
- [Kind](https://kind.sigs.k8s.io/)

The Makefile downloads project-scoped versions of Kustomize,
controller-gen, setup-envtest, and golangci-lint into `bin/` as needed.

## Clone and build

```sh
git clone https://github.com/nekomeowww/rc.git
cd rc
make build
```

`make build` regenerates manifests and Go code, formats and vets the source, and
builds the following binaries under `bin/`:

- `manager`, the Kubernetes controller manager
- `rcctl`, the user-facing CLI
- `rc-kube`, the in-Pod process supervisor

Run `make help` to list the available targets.

## Development with Kind

Kind provides an isolated Kubernetes API for controller development and e2e
tests. Create a reusable single-node development cluster with:

```sh
make setup-kind KIND_CLUSTER=rc-dev
kubectl cluster-info --context kind-rc-dev
```

### Local storage

rc needs PVC cloning for Repository-to-Worktree and
Environment-to-Workspace flows. For local macOS and Linux development,
`setup-kind` installs the SIG Storage
[`csi-driver-host-path`](https://github.com/kubernetes-csi/csi-driver-host-path)
and creates the `csi-hostpath-sc` StorageClass.

To add the driver to an existing Kind cluster, run:

```sh
make setup-csi-hostpath KIND_CLUSTER=rc-dev
```

Keep these local-development constraints in mind:

- Use a single-node Kind cluster.
- The driver is for tests only; do not use it for production data.
- A clone is a full file copy, so large repositories take more time and space.
- Volumes are lost when the Kind cluster or driver Pod is recreated.

Production deployments must provide a CSI StorageClass that supports PVC
cloning.

Make sure `kubectl` points at `kind-rc-dev` before using Make targets that act on
the current context:

```sh
kubectl config use-context kind-rc-dev
make install
make run
```

`make run` starts the controller on the host and uses the current kubeconfig.
Keep it running in one terminal, then use another terminal for resources and
`rcctl` commands:

```sh
go run ./cmd/rcctl --context kind-rc-dev -n default repo list
kubectl get repositories,worktrees,workspaces,agentprocesses -A
```

The setup commands derive a kubeconfig for the named Kind cluster before
installing the driver, so they do not depend on the current `kubectl` context.

When finished, delete the development cluster:

```sh
make cleanup-kind KIND_CLUSTER=rc-dev
```

## Run tests

Run the unit and envtest suite with:

```sh
make test
```

This target regenerates manifests and DeepCopy code, formats and vets Go source,
downloads the matching envtest Kubernetes binaries, and runs all non-e2e Go
tests.

Run lint separately:

```sh
make lint
```

The e2e suite creates an isolated Kind cluster, installs the CSI hostpath driver,
builds and loads the manager image, deploys rc, runs the tests, and removes the
cluster:

```sh
make test-e2e
```

Use a different isolated cluster name when needed:

```sh
make test-e2e KIND_CLUSTER=rc-test-local
```

If a failed or interrupted run leaves the cluster behind, clean up that exact
cluster explicitly:

```sh
make cleanup-test-e2e KIND_CLUSTER=rc-test-local
```

Do not run the e2e suite against a development or production Kubernetes cluster.

## Test development images in Kind

Build both runtime images and load them into an existing Kind cluster:

```sh
export KIND_CLUSTER=rc-dev
export IMG=rc-controller:dev
export RUNNER_IMG=rc-runner:dev

make docker-build IMG="$IMG"
make docker-build-runner RUNNER_IMG="$RUNNER_IMG"
kind load docker-image "$IMG" "$RUNNER_IMG" --name "$KIND_CLUSTER"
make deploy IMG="$IMG" RUNNER_IMG="$RUNNER_IMG"
```

Check the deployed controller with:

```sh
kubectl rollout status deployment/rc-controller-manager -n rc-system
kubectl logs -n rc-system deployment/rc-controller-manager -c manager -f
```

Use `make undeploy` to remove the manager and `make uninstall` to remove the
CRDs. Removing CRDs also removes every corresponding custom resource, so delete
only from the intended Kind cluster.

## Code generation

Kubebuilder owns part of the repository layout. Preserve all
`// +kubebuilder:scaffold:*` markers and use Kubebuilder commands when adding
APIs or webhooks.

After editing API types or Kubebuilder markers, run:

```sh
make manifests
make generate
```

Do not edit generated outputs directly:

- `config/crd/bases/*.yaml`
- `config/rbac/role.yaml`
- `config/webhook/manifests.yaml`
- `**/zz_generated.*.go`
- `PROJECT`

For Go changes, run the repository's standard fix and verification sequence:

```sh
make lint-fix
make test
```

## Project layout

rc uses Kubebuilder's multi-group layout for Repository and Workspace APIs,
while configuration APIs remain in the original API package:

```text
cmd/                         manager, rcctl, and rc-kube entry points
api/v1alpha1/                Credential and AgentCredential APIs
api/repositories/v1alpha1/   Repository, Worktree, and RepositoryExec APIs
api/workspaces/v1alpha1/     Environment, Workspace, and AgentProcess APIs
internal/controller/         reconciliation logic by API group
internal/cli/rcctl/          rcctl command implementation
internal/rckube/             in-Pod process supervisor
config/                      generated and hand-written Kubernetes manifests
docs/                        architecture decisions and runtime design
test/e2e/                    isolated Kind e2e suite
```

Read [`docs/design/workspace-runtime.md`](docs/design/workspace-runtime.md)
before changing Workspace lifecycle, credential projection, process retention,
or PVC cloning behavior. The ADRs under [`docs/adr/`](docs/adr/) record the
Repository and Worktree storage decisions.

## Pull requests

Before opening a pull request:

1. Keep the change focused and explain the user-visible behavior.
2. Add or update tests for changed behavior.
3. Regenerate manifests and DeepCopy code when API types or markers change.
4. Run `make lint` and `make test`.
5. Run `make test-e2e` for controller, RBAC, deployment, or cluster integration
   changes.
6. Update README, samples, design docs, or ADRs when commands, APIs, lifecycle
   rules, or operational requirements change.

When reporting a storage-related test result, include the Kubernetes version,
CSI driver, StorageClass, access modes, and whether PVC cloning was exercised.
