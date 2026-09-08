---
name: setup-rc-locally
description: Create a disposable local Kind cluster with clone-capable storage and install rc. Use only when the user explicitly asks for a local rc cluster; do not use for an existing or production cluster.
---

# Set up rc locally

Create a disposable local rc environment on macOS or Linux. This workflow uses Kind plus the SIG Storage CSI hostpath driver; it is for development/CI only, not production data.

## Authorization and preflight

- Confirm the user explicitly wants a local cluster and state the proposed Kind cluster name. Creating it, changing kubeconfig context, loading images, and installing the CSI driver are mutations; obtain approval immediately before doing them.
- Check Docker, Kind, kubectl, Git, and the repository checkout. Inspect `kind get clusters` before choosing a name; never reuse or delete an existing cluster without the user's direction.
- Explain that Kind's default `local-path` StorageClass cannot clone PVCs. rc's repository Worktrees and prepared Environments need cloning, so use the supplied `csi-hostpath-sc` driver instead.

## Create and install

From the rc checkout, run the repository-supported setup after approval:

```sh
make setup-kind KIND_CLUSTER=<cluster-name>
```

Verify `csi-hostpath-sc` with `kubectl --context kind-<cluster-name> get storageclass csi-hostpath-sc`, then install a release with the context pinned:

```sh
kubectl --context kind-<cluster-name> apply -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
kubectl --context kind-<cluster-name> rollout status deployment/rc-controller-manager -n rc-system
```

For source changes, build and load matching controller and runner images into that exact Kind cluster, then deploy with those explicit image tags. Do not silently switch the user's current kube context; `kubectl config use-context kind-<cluster-name>` is an opt-in convenience only after explicit approval.

Install the client with `go install github.com/nekomeowww/rc/cmd/rcctl@latest`, verify the controller rollout and rc CRDs, and identify the selected context and StorageClass for later `$use-rc` commands. Do not run cleanup unless explicitly requested; Kind/CSI storage is intentionally disposable.
