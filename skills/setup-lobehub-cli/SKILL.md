---
name: setup-lobehub-cli
description: Authenticate and run LobeHub CLI inside a persistent rc Workspace. Use when an rc Workspace needs lh login or lh connect; do not use to import a host LobeHub credential file.
---

# Set up LobeHub CLI in rc

Set up LobeHub CLI (`lh`) inside a persistent named rc Workspace. Do not import a host's `~/.lobehub/credentials.json` as a process Credential: current LobeHub CLI credentials are encrypted using hostname and OS user identity and normally cannot be decrypted in the Workspace.

First apply `$use-rc` to establish client installation, kubeconfig/context/namespace selection, and rc readiness. If no usable cluster exists, ask the user to prepare or select one; if rc is absent, route to `$setup-rc`. Do not create or change either without explicit approval.

1. Require a namespace when the Workspace name is ambiguous; otherwise identify its namespace through read-only discovery. Inspect the requested Workspace, its readiness, runner image, default cwd, and existing WorkspaceExecs. Confirm `lh` exists in the runner. Use a named Workspace because the login must remain in its persistent home. If `lh` is absent, report the runner-image requirement and ask before changing the image or installing software.
2. Creating a WorkspaceExec with `exec` mutates the cluster. If the request has not already explicitly authorized Workspace process creation, obtain approval before checking the existing login with `rcctl -n <namespace> exec <workspace> -- lh whoami --json`. If it is valid for the intended account, reuse it. If a different account is logged in, tell the user which account is active and ask before replacing it.
3. With explicit user approval, start attached `lh login` as a WorkspaceExec. Capture the exact browser URL and short-lived pairing code from its live output, give them to the user immediately, and wait for browser authorization. Never invent, alter, or reuse a code, and do not store it in source or durable documentation.
4. Continue observing that same process to its terminal result, then verify with `lh whoami --json`. Only after success may you start `lh connect`.

Run `lh connect` as a detached rc WorkspaceExec, not with `lh connect --daemon`, so rc owns observation, logs, reconnect, and stop lifecycle. Explicitly select the AgentCredential and any process credentials required by child work. Keep the default LobeHub home path unless every login and projection intentionally uses the same custom path.

Use the same existing Workspace for login, verification, and the device bridge.
`run` creates a new Workspace, so it is not the command for this sequence. Put
rcctl flags before the required positional Workspace, even if a Workspace default
is saved. Adapt credential names to those already available and authorized:

```sh
rcctl -n <namespace> exec -it <workspace> -- lh login
rcctl -n <namespace> exec <workspace> -- lh whoami --json
rcctl -n <namespace> exec -d --agent-credential <agent-credential> --credential <process-credential> <workspace> -- lh connect --device-id <device-id>
```

Run the login through a tool that yields live output while it remains active;
complete the browser authorization and account verification before starting the
bridge. Omit credential flags when no child work requires them. Observe the
bridge with `rcctl -n <namespace> ps --workspace <workspace>` and
`rcctl -n <namespace> logs <execution-id>`. Use `attach <execution-id>` to
reconnect to a live execution and `stop <execution-id>` to stop it; a disconnect
does not restart or stop the bridge.
