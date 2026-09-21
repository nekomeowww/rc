# Workspace dependency cache options

Status: comparison for discussion. No new storage model is selected or implemented.

A Workspace has a persistent home volume and a replaceable runtime Pod.
Worktrees have separate source volumes. The current Linux images put
rebuildable caches in `/tmp/cache`, outside those persistent volumes.

A PVC requests persistent storage from Kubernetes. A Pod can mount that storage
again after replacement. Persistence does not imply fast local storage or
concurrent access from different nodes.

## Choices

| Model | Location | Multiple Worktrees in one Workspace | Different Workspaces | Survives Pod replacement | Cost and limits | Current support |
| --- | --- | --- | --- | --- | --- | --- |
| Container-local cache | `/tmp/cache` | Shared content store, separate virtual stores | Independent | No | Local I/O, but dependency links can break after replacement | Runner default |
| Persistent home cache | Existing home PVC | Shared content store, separate virtual stores | Independent homes | Yes | No additional PVC; competes with home data and follows home snapshot policy | Manual configuration |
| Workspace cache PVC | Separate PVC owned by one Workspace | Shared content store, separate virtual stores | Independent | Yes | Separate capacity, storage class, and cleanup policy; extra volume lifecycle | Proposal |
| Shared cache PVC | Common volume for several Workspaces | Shared content store, separate virtual stores | Shared within a defined trust boundary | Yes | Cross-node access, concurrent writes, quotas, and cleanup need explicit support | Proposal |

Creating another PVC with the same network StorageClass does not itself improve
I/O speed. A separate cache PVC primarily changes ownership, capacity, snapshot,
and cleanup policy. Compare it with the existing home PVC before adding API.

The observed ihome rc volumes use `tns-iscsi` with ReadWriteOnce. This mode
permits writable mounts on one node, including multiple Pods on that node.
It does not support simultaneous writable attachment across nodes. Shared-cache
design must not silently assume ReadWriteMany support.

## pnpm has two distinct stores

| Data | Purpose | Sharing boundary |
| --- | --- | --- |
| Content store | Reusable package contents | Multiple Worktrees within the selected trust boundary |
| Virtual store | Dependency graph and links for a checkout | One independent directory per Worktree |

Sharing the content store saves downloads. It does not remove all per-checkout
dependency setup. Sharing one virtual-store directory between independent
Worktrees can mix incompatible dependency graphs.

Source and `node_modules` entry links can survive on a Worktree PVC while their
virtual-store targets disappear with the Pod. Persisting only the content store
does not preserve those targets. Persist both stores for complete dependency
availability after replacement, or define an explicit reconstruction step.

## Policies to decide before implementation

- Scope: one Workspace or several Workspaces in the same trust boundary.
- Retention: Pod replacement, Workspace deletion, and explicit cache cleanup.
- Snapshots: whether an Environment commit contains the cache.
- Paths: stable mount path and Worktree-specific virtual-store identity.
- Capacity: storage class, initial size, expansion, and full-volume behavior.
- Coordination: which owner can remove cache entries while processes use them.
- Recovery: detect missing targets before declaring dependencies usable.
- Portability: explicit support for Linux, Windows, and native macOS runtimes.

One Workspace with several Worktrees already covers the original AIRI use case.
Cross-Workspace sharing is not required for that workflow. Persistent-home
configuration can address Pod replacement without introducing a cache resource.

## Evaluation

Compare container-local, home-PVC, and separate-PVC storage with the same lockfile,
runner image, node, storage backend, and pnpm version. Record these measurements:

1. Cold dependency installation.
2. A second Worktree with a warm content store.
3. Dependency resolution after Pod replacement.
4. Build and typecheck filesystem workloads.
5. Capacity growth, cleanup, and active-consumer protection.

No performance benchmark is included in this comparison. Use isolated test
Workspaces for future measurements; do not clear live dependency stores.

## Evidence locations

- `Dockerfile.runner`: disposable `XDG_CACHE_HOME=/tmp/cache` default.
- `images/openai-codex-cli/Dockerfile`: pnpm content-store default.
- `internal/rcplatform/pods.go`: home PVC and runtime emptyDir mounts.
- `api/workspaces/v1alpha1/workspace_types.go`: no separate cache storage field.
- `skills/use-rc/SKILL.md`: per-Worktree virtual stores and cache recovery.
