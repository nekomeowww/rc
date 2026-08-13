# Remote Credentials

This context names the credential resources managed by rc without exposing the
secret material itself.

## Language

**Agent Credential**:
A credential file for a specific agent type, represented by a reference to one
entry in a same-namespace Secret.
_Avoid_: Generic credential

**Agent Type**:
The agent implementation that determines the format of an Agent Credential.

**Credential**:
A non-agent credential whose type describes how consumers interpret the
referenced secret data.
_Avoid_: Agent credential

**Secret Key Reference**:
The name of a same-namespace Secret and the key of the data entry containing the
credential material.
