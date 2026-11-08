# macOS integration failures

Check OS/op/app versions, the actual error, and whether the unlocked app has CLI integration enabled. For intended desktop integration, try command-scoped `OP_BIOMETRIC_UNLOCK_ENABLED=true` when supported; do not force it on headless/machine-identity flows.

On affected macOS versions, guide the user to Privacy & Security → Files & Folders → the actual terminal/editor/agent app → **Data shared by 1Password and affiliated apps**; restart that app if needed. The CLI community report describes the environment workaround; the official plugin documents the app-data permission for MCP, so verify applicability to the observed CLI failure.

Full Disk Access is not routine: consider it only when a reproduced failure and vendor guidance establish narrower access is insufficient, explaining the broader grant first. Do not reset privacy controls or extract sessions to bypass approval.

[CLI report](https://www.1password.community/1password-at-work-58/1password-cli-is-not-working-on-golden-gate-macos-27-25344) · [Official permission guidance](https://github.com/1Password/1password-claude-plugin#step-4-allow-claude-code-to-access-1password-data-macos-27-golden-gate) · [CLI integration](https://developer.1password.com/docs/cli/app-integration/)
