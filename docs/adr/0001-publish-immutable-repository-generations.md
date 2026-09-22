---
status: superseded
---

# Superseded: do not publish Repository generations

This decision was superseded by the independent parent/child PVC model. A
Repository owns one mutable parent PVC. A Worktree clones that PVC at creation
time. Every Worktree reuses the cloned working-tree root and applies its branch
or ref there, avoiding a redundant checkout on network storage. Generation
publication and generation garbage collection are intentionally not part of
the current design.
