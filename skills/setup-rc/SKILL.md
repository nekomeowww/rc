---
name: setup-rc
description: Install and bootstrap rc on an existing Kubernetes cluster, including optional GitHub and Codex credentials. Use when rc is absent or the user asks to prepare an existing cluster for rc; use setup-rc-locally for a new disposable local cluster.
---

# Set up rc

Prepare an existing Kubernetes cluster for rc. This skill does not create a cluster; route a requested disposable Kind cluster to `$setup-rc-locally`.

## Discover, then obtain approval

1. Inspect kubeconfig context, connectivity, namespaces, rc CRDs/controller, and available StorageClasses without exposing credential contents. Select an existing namespace or propose an isolated one. Verify the chosen StorageClass supports CSI PVC cloning from CSI/vendor documentation or an approved clone probe; do not infer it merely from dynamic provisioning.
2. If an installation is already healthy, do not reinstall it. Report the detected state and proceed only with requested bootstrap work.
3. Before applying manifests, creating a namespace, or importing credentials, summarize the exact context, namespace, StorageClass, and resources to be changed, then obtain explicit user approval.

## Install and verify

Install the released operator into the selected context with:

```sh
kubectl apply -f https://github.com/nekomeowww/rc/releases/latest/download/install.yaml
kubectl rollout status deployment/rc-controller-manager -n rc-system
```

Install the client with `go install github.com/nekomeowww/rc/cmd/rcctl@latest`, then confirm `rcctl --help` and the rc CRDs. For a checkout or pinned release, use matching controller and runner images rather than mixing an arbitrary controller with an incompatible runner.

When an approved isolated namespace is needed, create it idempotently before importing credentials:

```sh
kubectl create namespace <namespace> --dry-run=client -o yaml | kubectl apply -f -
```

## Optional credentials

Ask whether the user wants to import GitHub and/or Codex credentials; do not read, print, copy, or import either by default.

- For an HTTPS GitHub clone, first check `gh auth status --hostname github.com`. With approval, import it into the target namespace using `rcctl -n <namespace> credentials import --type github --name github-com`. This only authenticates Repository clone/sync operations, not Workspace `gh`.
- Detect a local Codex credential by checking whether `${CODEX_HOME:-$HOME/.codex}/auth.json` exists, without displaying it. With approval, import it using `rcctl -n <namespace> credentials import --type agent --agent codex --file <auth-file>`. If absent, report that a Codex login/credential is needed rather than inventing one.

Keep imported credentials namespaced and use them only for requested Workspaces/processes. Finish by listing rc resources in the target namespace and offer `$use-rc` for actual development work.
