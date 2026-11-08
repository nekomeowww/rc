# Kubeconfig adaptation

Start from the selected trusted kubeconfig/context. An Access endpoint/ST supplies no Kubernetes CA, TLS identity, or user: if missing, ask for approved materials and stop dependent rc checks. Inspect unfamiliar exec-plugin provenance before invoking it.

## Create

Default to a private derived file; preserve source bytes/current-context. For persistent edits, select one explicit write file, preserve unrelated entries, and coordinate writers/check the source digest before atomic owner-only replacement. Do not rely on a multi-file KUBECONFIG merge to choose the write target.

Capture selected identity/trust privately, never in tool/chat output:

```sh
umask 077
kubectl --kubeconfig <source> --context <source-context> config view \
  --minify --flatten --raw -o json > <new-private-staging-file>
```

Create the private parent first and avoid overwriting existing files. For multi-file input use the selected KUBECONFIG list. Flattening embeds sensitive certificates/keys; alternatively resolve relative paths against each entry's original file. Preserve all user auth, CA/TLS, namespace and extension fields. Exec-plugin relative dependencies need separate inspection.

Deep-copy the selected cluster under an unused name and create the requested context referencing it and the intended user; never derive a Kubernetes user from the ST. Handle name collisions as explicit updates.

| Route | Copied cluster |
| --- | --- |
| Raw API TCP | `server: https://127.0.0.1:<port>`; original API `tls-server-name`; delete incompatible `proxy-url` |
| Remote SOCKS | Preserve API server/TLS identity; `proxy-url: socks5://127.0.0.1:<port>` |

Derive TLS identity from original API configuration, not the Access hostname. If ambiguous (e.g. loopback without TLS name), resolve it instead of guessing. Keep source/global current-context unchanged; consumers pass explicit file/context. A renamed context in a private minified file needs a matching private current-context.

## Update and verify

If another context shares the target cluster, clone it and repoint only the target before transport edits. Preserve shared users, trust, namespace and unrelated objects; repeat updates without duplicates. `set-cluster`/`set-context` merge fields, do not clone, and do not remove stale proxy settings automatically. No `use-context` is needed.

Compare source digest, unrelated objects, current-context, auth/trust and intended transport privately. Then verify bounded `/version`, Kubernetes identity, and namespace-specific rc access through the exact file/context. Access approval/version alone is insufficient; test streaming separately if needed. Clean temporary configs/listeners on all exits, retaining only requested persistent state.

[Kubeconfig guide](https://kubernetes.io/docs/tasks/access-application-cluster/configure-access-multiple-clusters/) · [API](https://kubernetes.io/docs/reference/config-api/kubeconfig.v1/)
