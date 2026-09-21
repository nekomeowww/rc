---
status: accepted
---

# Initialize Worktrees in cloned Repository roots

## Context

A Worktree PVC is a CSI clone of a Repository PVC. It already contains an
independent Git repository and a complete working tree. Creating a native
linked worktree inside that clone writes a second complete working tree to the
same network volume.

In an AIRI repository measurement, the linked checkout took about 37 seconds.
Checking out the same commit in the cloned root took about 9 seconds. Both
operations started from an isolated child PVC.

## Decision

The Worktree controller initializes the requested branch or ref directly in
the cloned Repository root. `status.worktreePath` points to `/repository` for
new Worktrees. The `rcctl worktree` resource and command names remain stable
because they describe the rc isolation unit, not the Git linked-worktree
implementation.

Existing Worktrees keep their recorded path. Workspace and WorktreeExec mounts
therefore continue to support linked worktrees created by older controllers.

## Consequences

- Worktree creation avoids a second full working-tree materialization.
- The child PVC remains the isolation and lifecycle boundary.
- Branches, refs, detached HEAD, orphan branches, and no-checkout mode remain
  available.
- Git linked-worktree locks need no runtime action. The isolated root has no
  linked-worktree metadata that Git can prune.
