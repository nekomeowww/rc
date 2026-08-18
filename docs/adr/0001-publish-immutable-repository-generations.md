---
status: superseded
---

# Superseded: do not publish Repository generations

This decision was superseded by the independent parent/child PVC model. A
Repository owns one mutable parent PVC. A Worktree clones that PVC at creation
time and then runs `git worktree add` inside the child-local Git repository.
Generation publication and generation garbage collection are intentionally not
part of the current design.
