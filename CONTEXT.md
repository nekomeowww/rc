# rc

This context names the repository, credential, and workspace runtime concepts
managed by rc.

## Language

### Repositories

**Repository**:
A named, persistent Git parent PVC whose content can be used to create
independent Worktrees.

**Repository Sync**:
A request to make the Repository parent PVC match its configured remote and Git
Ref, replacing local changes in that parent PVC.
_Avoid_: Pull, refresh

**Git Ref**:
The configured full Git reference or commit that determines a Repository's
synchronized content. When absent, it means the remote's default branch.

**Repository Exec**:
A user-requested execution of an arbitrary command against a Repository.
_Avoid_: Repository Run

**Worktree**:
An independent child PVC cloned from a Repository parent PVC. The child owns a
complete Git repository and contains a native Git worktree created with
`git worktree add`; its reflog, stash, refs, and working files are independent
from the parent.

**Worktree Exec**:
A user-requested execution of an arbitrary command against a Worktree child
PVC.

### Remote Credentials

**Agent Credential**:
A credential file for a specific agent type, represented by a reference to one
entry in a same-namespace Secret.
_Avoid_: Generic credential

**Agent Type**:
The agent implementation that determines the format of an Agent Credential.

**Credential**:
A non-agent credential whose type describes how consumers present the
referenced secret data to a remote system.
_Avoid_: Agent credential

**Credential Type**:
The authentication or request presentation mechanism of a Credential. A
token's origin, such as OAuth, does not determine its Credential Type.

**SSH Private Key**:
A Credential Type that authenticates an SSH connection with a private key.

**SSH Configuration Fragment**:
Non-secret OpenSSH client configuration carried by an SSH Private Key
Credential and exposed only while a process uses that Credential.
_Avoid_: SSH options

**HTTP Basic Authentication**:
A Credential Type that presents a username and a secret password or token
through HTTP Basic authentication.
_Avoid_: Basic token

**HTTP Bearer Token**:
A Credential Type that presents a secret token through the HTTP Authorization
header using the Bearer scheme.
_Avoid_: OAuth token

**HTTP Headers**:
A Credential Type that presents one or more named secret values as HTTP request
headers.

**OAuth Access Token**:
A token obtained through OAuth. It is represented by the HTTP Credential Type
required by the remote system rather than treated as a separate Credential
Type.
_Avoid_: OAuth credential

**Secret Key Reference**:
The name of a same-namespace Secret and the key of the data entry containing the
credential material.

### Workspace Runtime

**Workspace Environment**:
A reusable source from which Workspaces receive their Runner Image and initial
writable user home state. A Workspace resolves at most one explicit Workspace
Environment.
_Avoid_: Workspace Template

**Runner Image**:
The execution image containing `rc-kube`, `rcctl`, Git, a shell, certificates,
system libraries, and common system tools. Workspace Environment images retain
the same runtime contract while selecting a different system foundation.
_Avoid_: Repository worker image, Workspace base image

**Environment Draft**:
The mutable working copy of a Workspace Environment that can be edited and
tested without changing the environment used by new Workspaces.

**Environment Commit**:
The operation that promotes an Environment Draft to the current Environment
Revision.
_Avoid_: Publish environment

**Environment Revision**:
A committed state of a Workspace Environment. Existing Workspaces retain the
revision from which they were created when a later revision is committed.

**Workspace Setup**:
A reusable, versioned capability bundle that a Workspace attaches in addition
to its environment foundation. It provides tools and their process environment
without becoming part of the Workspace's writable user home.
_Avoid_: Toolchain resource, Environment layer, setup script

**Setup Revision**:
An immutable state of a Workspace Setup selected by a Workspace. Existing
Workspaces retain their resolved Setup Revisions until explicitly upgraded.

**Workspace**:
A named, persistent development machine with independent writable user state.
It combines one resolved environment foundation, zero or more Workspace Setups,
code mounts, credentials, and compute capacity for concurrent Agent Processes.
_Avoid_: Agent Pod, workspace template

**Temporary Workspace**:
A Workspace created by rcctl because a run explicitly requests `--temporary`
or declares topology without selecting an existing Workspace. It has the same
persistence and cleanup rules as any other Workspace.
_Avoid_: Ephemeral Workspace

**Workspace Mount**:
A named association between a Workspace path and either a writable Worktree or
a read-only Repository.

**Agent Process**:
An rc-managed, at-most-once command running against a Workspace or Environment
Draft. It may use a terminal, but it is not an agent-native session.
_Avoid_: Agent Exec as a distinct resource, agent session

**Agent Exec**:
The short CLI operation for running a non-terminal Agent Process synchronously.
It is not a separate Kubernetes resource kind.

**Agent Home**:
Persistent configuration, cache, and session state shared by Agent Processes
that use the same Agent Type and Agent Credential in one Workspace. Credential
material is not part of the Agent Home.
