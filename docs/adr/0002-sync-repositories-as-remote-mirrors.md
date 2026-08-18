---
status: accepted
---

# Sync Repositories as remote mirrors

Repository Sync treats the configured remote and Git Ref as authoritative. A
sync fetches the remote, resolves the configured full Git ref or commit (or the
remote default branch when no ref is configured), resets the Repository parent
PVC with `git reset --hard`, and removes untracked and ignored files with
`git clean -ffdx`.

This deliberately discards local changes in the parent instead of merging or
rebasing them. Repository Exec remains the explicit mechanism for arbitrary Git
workflows. Existing Worktrees are independent child PVCs and are not changed by
Repository Sync; a Worktree may run its own Git command or future Worktree Sync
operation when it needs remote updates.
