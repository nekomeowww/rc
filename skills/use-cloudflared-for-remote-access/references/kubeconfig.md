# Create and update a route-specific kubeconfig context

Cloudflare Access authenticates the transport. It does not supply a Kubernetes
identity. Start from the user's selected trusted kubeconfig/context, or ask for
the approved Kubernetes credential source, CA and API TLS identity if absent.
Do not request secret contents in chat, invent credentials, or disable certificate
verification. With only an Access endpoint, report transport preparation separately
and stop dependent rc verification until Kubernetes identity is available.

Treat a supplied kubeconfig as potentially executable: inspect the selected exec
plugin and its provenance before invoking it. Do not execute unknown auth helpers
merely to inspect metadata.

## Select a write target

- Default to a separate private derived file for temporary access. Source file(s)
  and their current-context remain unchanged. Persistent context creation/update
  uses the path and names authorized by the user; do not require another approval
  for the same requested edit.
- Resolve explicit `--kubeconfig`, the KUBECONFIG list, and normal discovery
  consistently. Use an explicit single write file; do not rely on multi-file merge
  rules to choose where a mutation lands. Preserve unrelated entries in that file.
- Record a source-file digest and inspect only selected non-secret metadata before
  adaptation. Recheck the source digest before replacing a persistent file to avoid
  overwriting concurrent edits. Use a private temp file and atomic replacement;
  preserve owner-only permissions. Reject symlink targets or ambiguous collisions.

## Copy identity and trust without revealing them

Use structured editing or kubectl's config commands, never regex substitutions.
One way to derive a selected context with certificate paths resolved is to capture
kubectl output privately, not through conversation/tool output:

```sh
# Create the private output directory first; never overwrite an unrelated file.
umask 077
kubectl --kubeconfig <source-file> --context <source-context> config view \
  --minify --flatten --raw -o json > <private-staging-file>
```

For multi-file input, use the selected KUBECONFIG list rather than pretending it
is one `--kubeconfig` filename. `--flatten` can embed sensitive certificate/key
material: capture it directly into an owner-only file, never print or commit it,
and delete a temporary derived config after use. Alternatively preserve file
references and resolve each relative CA/certificate/key path against the file that
originally defined that entry. Do not copy relative paths into a new directory
unchanged. Preserve existing `*-data`, token/tokenFile, client-certificate/key,
auth-provider, impersonation, exec, namespace, extensions, and TLS metadata.
Inspect exec-plugin relative arguments and dependencies separately: flattening
certificate paths does not make an arbitrary plugin portable.

## Create a new context

1. Inspect the selected source context, cluster, and user references. Choose unused
   destination context and cluster names. If a destination name already exists,
   inspect it and perform an authorized update rather than silently replacing it.
2. Deep-copy the source cluster under the distinct destination cluster name. Create
   the destination context pointing to that cluster and the intended existing user
   (or its private copy in a separate derived file), preserving namespace and other
   context fields. Do not synthesize a user from the Access Service Token.
3. Apply only route-specific transport changes to the copied cluster:

   | Origin | Destination cluster change |
   | --- | --- |
   | Raw API TCP | Set server to `https://127.0.0.1:<listener-port>`, preserve/derive the API's real `tls-server-name`, remove an incompatible `proxy-url` |
   | Remote SOCKS service | Keep the API server and TLS identity, set `proxy-url: socks5://127.0.0.1:<listener-port>` |
   | Working direct route | Preserve transport unless the user requested a change |

   Derive TLS identity from the original API certificate configuration/server, not
   the Access edge hostname. A source server already set to loopback without an
   explicit TLS identity may be insufficient; resolve it instead of guessing.
4. Keep the source/global `current-context` unchanged. Use explicit
   `--kubeconfig <destination-file> --context <destination-context>` on consumers.
   In a minified private file, set its current-context to an existing renamed
   context if necessary for internal consistency; this does not switch the source.

## Update an existing context

Inspect the target context's cluster consumers before changing server, proxy, or
TLS fields. If another context uses that cluster, clone it under a new unused
name and repoint only the target context. Do not modify the shared user to change
a transport. Update only the requested fields; retain trust, credentials,
namespace, and unrelated contexts. Repeatedly applying the same endpoint/port must
not accumulate duplicate contexts/clusters or change unrelated objects.

`kubectl config set-cluster` and `set-context` merge fields into named entries;
they do not automatically clone shared entries or remove a stale proxy. Use an
explicit unset or structured deletion for obsolete `proxy-url`. `use-context`
changes the file's default and is unnecessary when consumers pass `--context`.
Never dump raw config to verify a change.

## Verify the result

Compare parsed configuration privately and report booleans/digests rather than
secrets: source unchanged, unrelated contexts/clusters/users unchanged, expected
current-context, destination namespace/user/trust preserved, and exactly the
intended transport changes. Check both initial creation and a later port update,
including a shared-cluster target and repeated application.

Then run bounded `/version`, Kubernetes identity, and namespace-specific rc access
through the exact destination file/context and owned listener. Verify exec/attach
separately if needed. Version-only success, Access approval, or a modified file is
not proof of authenticated rc access. On failure, preserve the original config,
report the failed layer, and clean up temporary derived material/listeners.

Sources:

- [Organizing cluster access](https://kubernetes.io/docs/concepts/configuration/organize-cluster-access-kubeconfig/)
- [Configure access to multiple clusters](https://kubernetes.io/docs/tasks/access-application-cluster/configure-access-multiple-clusters/)
- [Kubeconfig API](https://kubernetes.io/docs/reference/config-api/kubeconfig.v1/)
