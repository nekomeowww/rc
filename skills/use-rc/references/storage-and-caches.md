# Storage and caches

Inspect a compatible Ready Workspace and the deployment's StorageClass, cloning support, access modes, resources, and node constraints. Do not guess names or assume Ready means suitable. rc rejects WaitForFirstConsumer classes; Repository/Worktree/Environment flows require PVC cloning.

```sh
kubectl get storageclass -o custom-columns=NAME:.metadata.name,BINDING:.volumeBindingMode
```

Network storage can make file-heavy work slow. Measure locally, batch checks, use `rg`/`git grep`, and avoid tight polling; detach long commands when caller timeouts require it.

For Linux pnpm Workspaces, rebuildable caches may use local storage after checking image configuration and permissions:

```sh
rcctl -n <namespace> exec --no-env-passthrough --cwd /workspace/<mount> <workspace> -- pnpm install --store-dir=/tmp/cache/pnpm/store --virtual-store-dir=/tmp/cache/pnpm/virtual/<worktree>
```

Share the content store, not virtual stores between Worktrees. Follow repository version/lockfile/build policies; keep both flags on dependency-changing commands (pnpm 11+ does not read these from `.npmrc`). Turbopack requires a root containing the source and virtual store, or an in-project virtual store.

Runtime replacement can erase local caches. If links are stale, stop consumers, move only generated `node_modules` to an identified backup outside package globs, reinstall, and verify resolution. Preserve source/lockfiles; avoid defaulting to `--force`. Choose persistent stores when durability outweighs PVC I/O cost.
