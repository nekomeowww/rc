# Doctor and safe pruning (T-665)

This replaces the uncommitted T-664 implementation at baseline
`51d9ce86f84d9a704e4a23fde3546712589d9347`, in worktree `t664` on
`codex/t664-doctor-gc`. No commits, deployment or real-cluster access are required.

## Current integration status

T-663's canonical retention evaluator is **not present in this worktree**.
`audit.HistoryEvaluator` defines the integration boundary; production registration
passes nil. `doctor` works normally. A fresh `prune --dry-run` prints an empty plan
and an explicit `UNKNOWN` warning on stderr; execution fails closed. The execution
pipeline is tested using injected decisions, not a second retention implementation.

The adapter must delegate to the canonical evaluator for terminal/completedAt,
retain intent, effective target policy and attachment semantics. It receives typed
record and target objects plus the evaluation time, performs no I/O, returns false
for retained records and an error for unknown policy. A nil target means confirmed
absence, not permission to delete. Unsupported kinds must remain ineligible unless
an applicable canonical evaluator exists. Wire that adapter at `maintenance.Register`
when T-663 is integrated; use the same adapter for review and deletion.

`--history-for` is an **additional** age floor (default 168h). Shortening it can
never turn a canonical rejection into permission. Doctor's lifecycle projections
are diagnostic evidence; they do not define retention defaults or target policy.

## Commands

```sh
rcctl -n development doctor
rcctl doctor -A -o json
rcctl -n development prune --dry-run --history-for 336h
rcctl -n development prune --dry-run -o json > plan.json
# With the canonical adapter integrated, review the selected identities:
rcctl -n development prune
rcctl prune --from-plan plan.json --yes
```

Doctor reports runtime/storage failures, references, owner identity, deletion
blockers and PVC requests. Requested capacity is not actual usage or reclaimed
storage. Workspaces, Worktrees, Repositories, PVCs, Pods and Leases never become
prune actions. Missing permissions remain `unknown`; a successful list of another
kind can still prove that its referenced dependency is absent.

Prune previews support table/JSON/YAML, namespace or `-A`, and `--history-for`.
An explicit `yes` line confirms interactive pruning; EOF or another answer cancels.
`--yes` skips the prompt. `--dry-run` always prevents writes, even with `--yes`.
Warnings/prompts use stderr; structured stdout contains exactly one document.

## Small plans and bounded requests

`CleanupPlan` v1alpha2 contains only version, namespace, history policy, creation
and expiry times, and executable terminal-history identities (API/kind/name/UID/
resourceVersion). It contains no report, inventory, blocked/data candidates,
reference graph, capacity estimate or reconstructed JSON signature. Older plans
must be regenerated. JSON plans are selections, not signed capabilities.

Both fresh and saved execution paths do one uncached scan of 13 resource kinds
before confirmation. Saved identities must still be eligible and unchanged; new
eligible records are never added. Unrelated changes are allowed. Already absent
records and deletion-pending instances are resumable, but name reuse is rejected.
The saved scope/policy cannot be overridden, and its original one-hour maximum
lifetime is never renewed. Expiry is checked again after confirmation and before
DELETE. Partial visibility yields a non-executable fresh preview; saved-plan
revalidation and execution fail when safety evidence is unknown.

After confirmation, each candidate gets a fresh GET of itself, its direct target
and any named execution Job. Eligibility, completion age, finalizers, attachment
and canonical retain/target policy are rechecked. Every DELETE carries UID and
resourceVersion preconditions with orphan propagation. The result distinguishes
requested, observed absent, and pending deletion, including partial failures.

Kubernetes cannot atomically fence changes to a different object between GET and
DELETE. New incoming references after review and target-policy changes after the
last GET remain a cross-object race. This path deletes only terminal history and
orphans dependents; it never claims to delete or reclaim their data. Finalizers
are never stripped. Do not extend this path to data resources without a separate
server-side safety contract.

## Measurement and verification

The before/after request regression uses a counting client, excluding API discovery:

| Two eligible WorkspaceExec records, after review | Before | After |
| --- | ---: | ---: |
| LIST | 39 | 0 |
| GET | 4 | 6 |
| DELETE | 2 | 2 |

The two extra GETs reread target policy. The old execution rescanned all 13 kinds
`N + 1` times after review. The new whole review/execution path uses 13 LISTs,
`3N` GETs and `N` DELETEs for WorkspaceExec records that disappear immediately.
Named execution Jobs add at most one GET per candidate. Already absent/pending
records need only their own GET. The regression covers N = 1, 2 and 100.

Removed: capability-gap baseline test, old command-name negative assertion,
fake-client conditional-delete duplication, data-impact planning and full-snapshot
JSON equality. Expired/dry-run/saved-plan CLI cases share one table. Atomic UID/RV
races use the isolated envtest API; one CLI happy path covers confirmation, saved
selection and retry. Detailed doctor graphs stay in unit tests, while the HTTP
fixture contains only five objects.

Measured source sizes (line/byte counts include comments; no generated code):

| Scope | Before lines / bytes | After lines / bytes |
| --- | ---: | ---: |
| Audit + maintenance production Go (8 files) | 1,467 / 57,306 | 1,405 / 54,428 |
| Audit + maintenance/root tests | 857 / 36,790 | 886 / 37,297 |
| CLI fixture (13 objects → 5) | 692 / 16,929 | 22 / 6,880 |

Production shrank by 62 lines / 2,878 bytes; fixture bytes fell 59.4%. Tests grew
slightly because the smaller set of conditional-delete tests now includes both
real API races, bounded-request regression and canonical-policy boundary coverage.
The fixture stores one compact JSON response per line; the object reduction is
separate from formatting. Measurements were taken after all code checks.


```sh
go test ./... -count=1
go test ./internal/audit -run TestPruneRequestBudget -count=1 -v
KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.36 --bin-dir "$PWD/bin" -p path)" \
  go test -tags=integration ./internal/audit -run TestAPIConditionalDeletion -count=1 -v
make lint-fix
make lint
make test
```

Verified on 2026-10-07: full non-cached tests, the N=1/2/100 request regression,
all three envtest cases, `make lint-fix`, `make lint` (0 issues), and `make test`
passed. The initial envtest attempt used a relative asset path; the absolute
`$PWD/bin` command above fixes test-working-directory resolution.

All tests use fake clients, local HTTP fixtures or an isolated envtest API server;
no ihome or other real-cluster operations are involved.
