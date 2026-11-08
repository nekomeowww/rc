# Storage and dependency caches

Inspect the selected deployment's StorageClasses, binding mode, cloning support,
access modes, node constraints, and a compatible Ready Workspace before creating
storage. Do not guess a class or node selector from another deployment. Ready
alone does not prove a Workspace has suitable tools, free resources, or ownership.

rc rejects WaitForFirstConsumer storage with
`WaitForFirstConsumerUnsupported`. Repositories, Worktrees, and Environments
also require PVC cloning. Check the binding mode:

```sh
kubectl get storageclass -o custom-columns=NAME:.metadata.name,BINDING:.volumeBindingMode
```

Network-backed volumes can make file-heavy operations slow; measure the selected
runtime rather than carrying timings from another installation. Batch small
checks, bound broad searches, and use `git grep` or `rg`. Use a foreground exec
when it fits the caller's timeout, or a detached execution with logs and attach
for longer work. Avoid tight polling loops.

## pnpm caches in Linux Workspaces

Keep source on PVC and rebuildable dependencies in local storage. Updated Linux
images use `XDG_CACHE_HOME=/tmp/cache`; check this and directory permissions for
older/custom images.

```sh
rcctl -n <namespace> exec --no-env-passthrough --cwd /workspace/<mount> <workspace> -- pnpm install --store-dir=/tmp/cache/pnpm/store --virtual-store-dir=/tmp/cache/pnpm/virtual/<worktree-id>
```

- Use the Worktree resource name for `<worktree-id>`. Share the content store
  within a Workspace, but give each Worktree its own virtual store.
- Install from the repository's required root and follow its pnpm, lockfile, and
  build-script policies. Keep both path flags for `install/add/remove/update`;
  pnpm 11+ does not read store settings from `.npmrc`.
- Serialize dependency changes per Worktree. All consuming processes must see
  the same virtual-store path.
- Next.js Turbopack does not resolve packages from a virtual store outside the
  project root. For a Turbopack app, set `turbopack.root` to a directory that
  contains both, or keep that Worktree's virtual store inside the Worktree.
- Local caches can disappear when the runtime is replaced. Keep credentials out
  of them and never clear dependencies while processes are using them.

### Cache recovery

After runtime replacement, check dependency links: `Already up to date` does not
guarantee a missing virtual store was rebuilt. For stale links or path migration,
stop affected processes, move only that Worktree's generated root/package
`node_modules` directories to a unique PVC backup outside workspace package globs,
then reinstall with the same flags and verify runtime resolution. Preserve source
and lockfiles; remove the identified backups only when authorized.

rc does not automate recovery. Avoid defaulting to `--force`, which can fetch
other-platform optional dependencies. For durability instead, select persistent
content and virtual stores, accepting PVC I/O costs.

See [pnpm virtualStoreDir](https://pnpm.io/settings/node-modules#virtualstoredir).
