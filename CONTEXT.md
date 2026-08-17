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
A non-agent credential whose type describes how consumers present the
referenced secret data to a remote system.
_Avoid_: Agent credential

**Credential Type**:
The authentication or request presentation mechanism of a Credential. A
token's origin, such as OAuth, does not determine its Credential Type.

**SSH Private Key**:
A Credential Type that authenticates an SSH connection with a private key.

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
