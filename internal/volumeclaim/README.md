# Managed PVC identity

Use this package when a controller allocates a managed persistent volume or a
client creates an rc resource that will need one.

- `Name` computes the new typed PVC name; it does not locate existing data.
- `Resolve` prefers the recorded status reference, then an owned legacy PVC,
  then the typed name. Persist the result before exposing it to consumers.
- `CheckOwner` guards existing PVC use, promotion and deletion.
- `Preflight` checks the caller-selected role/revision before creating the CR.
  Callers skip resources without PVC storage (such as Darwin Workspace homes).
  Controllers still verify ownership to handle direct API writes and races.

Runtime mounts, clone sources and log readers use the resource's recorded
status. Do not call `Name` to reconstruct those references or use this package
for Git checkout paths, Pod names, Job names or Lease identities.

See [ADR-0005](../../docs/adr/0005-isolate-persistent-volume-claim-identities.md)
for compatibility policy and reproduction evidence.
