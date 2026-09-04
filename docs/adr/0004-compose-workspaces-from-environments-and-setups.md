---
status: accepted
---

# Compose Workspaces from one Environment and multiple Setups

ADR-0003 established the Workspace as a persistent development machine and the
Workspace Environment as the source of its Runner Image and writable user home.
Using the same Environment to carry every optional tool also couples the home
baseline, Runner Image, and tool selection. A Python version change, for
example, would require another complete Environment even when several
Workspaces could otherwise share the same base.

A Workspace directly composes one resolved environment foundation and zero or
more Workspace Setups. The foundation comes from an explicitly referenced
Workspace Environment or the controller-provided blank environment. The
Environment supplies the Runner Image and the initial state cloned into the
Workspace's writable home. Each resolved Setup Revision supplies an
independently reusable, immutable capability such as Python, a native build
toolchain, or an SDK.

The Workspace remains the stateful runtime entity. It owns its writable home,
runtime lifecycle, process records, and claims on writable Worktrees. It also
records the Environment Revision, Runner Image, and ordered Setup Revisions
that were resolved when its topology was prepared. Runtime Pods are replaceable
carriers of that state and do not define Workspace identity.

Workspace Setups are additive and do not participate in writable-home cloning.
They are exposed read-only at stable paths and declare the process environment
needed to use their tools, including executable search paths and tool-specific
variables. A Setup must not depend on modifying the container root filesystem
or writing back to its published revision. Compatibility requirements such as
operating system, architecture, libc, and prerequisite capabilities are checked
when the Workspace composition is resolved.

Workspace Environment and Workspace Setup references are siblings in the
Workspace specification. Environments do not transitively select Setups. This
keeps the final composition visible on the Workspace and allows one Setup to be
reused with every compatible Environment. If reusable collections of
Environment, Setup, mount, credential, and compute defaults are needed later,
they belong in a separate Workspace Profile rather than introducing hidden
inheritance between Environment and Setup resources.

Environment and Setup updates do not mutate existing Workspaces implicitly. An
Environment update may change both the Runner Image and the source of writable
home state, so adopting it is an explicit Workspace upgrade or migration. A
Setup update changes read-only runtime topology; adopting it replaces the
runtime Pod while preserving the Workspace home and Worktrees. Deleting a
Workspace deletes its owned runtime state and releases Worktree claims, but
does not delete referenced Environments or Setups.

## Considered Options

- **Merge multiple Workspace Environments.** Rejected because a Workspace has
  one writable home and one Runner Image. Multiple clone sources leave file
  conflicts, write ownership, image selection, and commit destinations
  undefined.
- **Bake every tool into the Runner Image.** Rejected because each tool or
  version combination requires another image and prevents independent reuse and
  upgrades.
- **Install missing system packages as root in a running Workspace.** Rejected
  because the result is not reproducible, root-filesystem changes disappear
  with the Pod, and normal Workspace processes intentionally cannot elevate
  privileges.
- **Keep every optional tool in the mutable Environment home.** Retained as a
  migration path, but not as the composition model. It copies unrelated tools
  into every Workspace clone and couples capability upgrades to the home
  baseline.

## Consequences

- Workspace reconciliation must resolve, validate, pin, mount, and activate an
  ordered set of Setup Revisions.
- Setup ordering and executable or environment-variable conflicts require a
  deterministic policy; ambiguous conflicts should be rejected unless the
  Workspace explicitly chooses precedence.
- Adding, removing, or upgrading a Setup is a Workspace topology change and
  follows the same active-process safeguards as code-volume changes.
- The existing Workspace Environment remains responsible for mutable user
  configuration, caches, and agent state. Setups remain immutable and reusable.
