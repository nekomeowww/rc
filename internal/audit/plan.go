package audit

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const planVersion = "rc.ayaka.io/cleanup-plan/v1alpha2"

// Review performs exactly one fresh scan before confirmation. Saved plans are
// untrusted selections: only still-eligible, unchanged identities survive. New
// eligible records are never added to a saved selection. Unrelated changes do
// not invalidate it. Partial visibility produces an empty, non-executable preview.
func Review(ctx context.Context, reader client.Reader, namespace string, policy HistoryPolicy, saved *CleanupPlan, evaluate HistoryEvaluator, now time.Time) (ReviewedPlan, error) {
	started := time.Now()
	review := ReviewedPlan{evaluate: evaluate}
	if policy.HistoryFor <= 0 {
		return review, fmt.Errorf("history age must be positive")
	}
	if saved != nil {
		if err := validatePlan(*saved, now); err != nil {
			return review, err
		}
		if saved.Namespace != namespace || saved.Policy != policy {
			return review, fmt.Errorf("saved plan scope or policy differs from the requested review")
		}
	}
	scanPolicy := DefaultPolicy()
	scanPolicy.HistoryFor = policy.HistoryFor
	inventory, err := scanInventory(ctx, reader, namespace, scanPolicy, now)
	if err != nil {
		return review, err
	}
	review.plan = CleanupPlan{Version: planVersion, Namespace: namespace, Policy: policy, CreatedAt: inventory.ObservedAt, ExpiresAt: metav1.NewTime(inventory.ObservedAt.Add(time.Hour)), Candidates: []ObjectRef{}}
	for _, observation := range inventory.Coverage {
		if !observation.Complete {
			review.Unknowns = append(review.Unknowns, observation.Kind+" visibility unknown: "+observation.Error)
		}
	}
	if evaluate == nil {
		review.Unknowns = append(review.Unknowns, "canonical history evaluator unavailable; integrate T-663 before pruning")
	}
	if len(review.Unknowns) > 0 {
		if saved != nil {
			return review, fmt.Errorf("cannot revalidate saved plan: %v", review.Unknowns)
		}
		return review, nil
	}
	g := newGraph(inventory)
	eligible := map[string]ObjectRef{}
	groups := map[string][]Resource{}
	for _, r := range inventory.Resources {
		if r.Kind != workspaceExecKind {
			continue
		}
		groups[key(executionTarget(r))] = append(groups[key(executionTarget(r))], r)
	}
	for _, records := range groups {
		target, _ := g.resolve(executionTarget(records[0]))
		removable, err := evaluateHistory(records, target.object, now, evaluate)
		if err != nil {
			return review, fmt.Errorf("retention unknown for %s: %w", key(executionTarget(records[0])), err)
		}
		for i, r := range records {
			if !removable[i] || !historyEligible(r, policy, now) || historyReferenced(r, g) {
				continue
			}
			eligible[key(r.ObjectRef)] = r.ObjectRef
			review.plan.Candidates = append(review.plan.Candidates, r.ObjectRef)
		}
	}
	slices.SortFunc(review.plan.Candidates, func(a, b ObjectRef) int { return cmp.Compare(key(a), key(b)) })
	if saved != nil {
		for _, ref := range saved.Candidates {
			current, exists := g.byKey[key(ref)]
			// Already absent selections remain resumable.
			if !exists {
				continue
			}
			if current.UID == ref.UID && current.DeletingAt != nil {
				continue
			}
			if eligible[key(ref)] != ref {
				return review, fmt.Errorf("stale cleanup plan: changed or ineligible %s", key(ref))
			}
		}
		review.plan = *saved
		review.plan.Candidates = append([]ObjectRef{}, saved.Candidates...)
	}
	return review, validatePlan(review.plan, now.Add(time.Since(started)))
}

// historyEligible adds transport safety and the CLI's age floor to the canonical
// decision. No retention/target policy is inferred from diagnostic projections.
func historyEligible(r Resource, policy HistoryPolicy, now time.Time) bool {
	if !isHistory(r) || !r.Terminal || r.CompletedAt == nil || !older(*r.CompletedAt, now, policy.HistoryFor) || r.UID == "" || r.ResourceVersion == "" || r.DeletingAt != nil || len(r.Finalizers) > 0 || r.AttachedClients > 0 {
		return false
	}
	return true
}

func evaluateHistory(records []Resource, target client.Object, now time.Time, evaluate HistoryEvaluator) (map[int]bool, error) {
	if evaluate == nil {
		return nil, fmt.Errorf("canonical history evaluator unavailable")
	}
	typed := make([]workspacesv1alpha1.WorkspaceExec, len(records))
	for i := range records {
		record, ok := records[i].object.(*workspacesv1alpha1.WorkspaceExec)
		if !ok {
			return nil, fmt.Errorf("unsupported history object %T", records[i].object)
		}
		typed[i] = *record.DeepCopy()
		if ownerUIDMismatch(&typed[i], target) {
			typed[i].Spec.Retain = true
		}
	}
	plan, err := evaluate(typed, target, now)
	if err != nil {
		return nil, err
	}
	removable := make(map[int]bool, len(plan.Remove))
	for _, i := range plan.Remove {
		if i < 0 || i >= len(records) || removable[i] {
			return nil, fmt.Errorf("canonical evaluator returned invalid removal index %d", i)
		}
		removable[i] = true
	}
	return removable, nil
}

func ownerUIDMismatch(record *workspacesv1alpha1.WorkspaceExec, target client.Object) bool {
	if target == nil {
		return false
	}
	for _, owner := range record.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.Kind == string(record.Spec.TargetRef.Kind) && owner.Name == record.Spec.TargetRef.Name {
			return owner.UID != "" && target.GetUID() != "" && owner.UID != target.GetUID()
		}
	}
	return false
}

func historyReferenced(r Resource, g graph) bool {
	if len(g.leases(r)) > 0 {
		return true
	}
	for _, incoming := range g.incoming(r) {
		// Events describe history; they are not live consumers or deletion authority.
		if incoming.Relation != "referenced-by:event" {
			return true
		}
	}
	for _, ref := range r.References {
		if ref.Relation == "execution-job" {
			if _, exists := g.resolve(ref.Target); exists {
				return true
			}
		}
	}
	return false
}

func executionTarget(r Resource) ObjectRef {
	for _, ref := range r.References {
		if ref.Relation == "execution" {
			return ref.Target
		}
	}
	return ObjectRef{}
}

func validatePlan(plan CleanupPlan, now time.Time) error {
	if plan.Version != planVersion {
		return fmt.Errorf("unsupported cleanup plan version %q", plan.Version)
	}
	if err := validatePlanTime(plan, now); err != nil {
		return err
	}
	if plan.Policy.HistoryFor <= 0 {
		return fmt.Errorf("history age must be positive")
	}
	seen := map[string]bool{}
	for _, ref := range plan.Candidates {
		if ref.APIVersion != workspaceAPI || ref.Kind != workspaceExecKind || ref.Namespace == "" || ref.Name == "" || ref.UID == "" || ref.ResourceVersion == "" || (plan.Namespace != "" && ref.Namespace != plan.Namespace) || seen[key(ref)] {
			return fmt.Errorf("invalid or duplicate terminal-history identity: %s", key(ref))
		}
		seen[key(ref)] = true
	}
	return nil
}

func validatePlanTime(plan CleanupPlan, now time.Time) error {
	if plan.CreatedAt.IsZero() || now.Before(plan.CreatedAt.Time) || !now.Before(plan.ExpiresAt.Time) || !plan.ExpiresAt.After(plan.CreatedAt.Time) || plan.ExpiresAt.Sub(plan.CreatedAt.Time) > time.Hour {
		return fmt.Errorf("cleanup plan is expired, from the future or has an invalid lifetime; generate a fresh plan")
	}
	return nil
}
