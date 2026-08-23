---
status: superseded
---

# Superseded: do not publish Repository generations

This decision was superseded by the independent parent/child PVC model. A
Repository owns one mutable parent PVC. A Worktree clones that PVC at creation
time. Explicit Worktrees run `git worktree add` inside the child-local Git
repository. Worktrees generated for a Workspace reuse the cloned working-tree
root and only create their unique branch, avoiding a redundant checkout on
network storage. Generation publication and generation garbage collection are
intentionally not part of the current design.
