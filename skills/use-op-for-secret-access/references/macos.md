# macOS desktop integration troubleshooting

Use only when the access device is macOS and the observed failure involves
1Password app integration. Do not prescribe macOS permissions on Linux, headless
hosts, or a working Service Account flow.

1. Inspect macOS, op, and desktop-app versions and the exact sanitized error.
   Confirm that the app is running/unlocked and CLI integration is enabled.
2. Where the installed CLI supports it and desktop integration is intended, try
   `OP_BIOMETRIC_UNLOCK_ENABLED=true` for the affected command first. Do not set it
   globally without need or use it to bypass approval. The linked macOS 27 report
   describes this as a workaround, not a universal fix.
3. On macOS versions showing the relevant control, inspect System Settings >
   Privacy & Security > Files & Folders > the actual terminal/editor/agent app >
   **Data shared by 1Password and affiliated apps** (wording may vary). Guide the
   user to enable the narrow relevant permission and restart that app if required.
   Identify the app hosting the command; granting access to a different terminal
   will not necessarily help. 1Password's official plugin documents this control
   for MCP app-data access; validate the actual CLI failure rather than assuming
   every integration error has the same cause.
4. Do not recommend Full Disk Access as a routine fix. Only discuss it when a
   reproduced remaining failure and applicable vendor guidance establish that the
   narrower permission cannot satisfy the required access. Explain the broader
   permission and obtain the user's decision before proceeding.

Do not reset privacy databases, copy app databases, extract session tokens, or
turn off security protections as a troubleshooting shortcut. Keep an interactive
approval pending and observable; unattended workflows need a supported machine
credential instead.

Sources:

- [macOS 27 CLI community report](https://www.1password.community/1password-at-work-58/1password-cli-is-not-working-on-golden-gate-macos-27-25344)
- [1Password official plugin, macOS permission setup](https://github.com/1Password/1password-claude-plugin#step-4-allow-claude-code-to-access-1password-data-macos-27-golden-gate)
- [CLI app integration](https://developer.1password.com/docs/cli/app-integration/)
