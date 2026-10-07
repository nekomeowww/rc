package audit

import (
	"context"
	"fmt"
	"time"

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
	for _, r := range inventory.Resources {
		if !isHistory(r) || !r.Terminal {
			continue
		}
		target, _ := g.resolve(executionTarget(r))
		allowed, err := historyEligible(r, target.object, policy, now, evaluate)
		if err != nil {
			return review, fmt.Errorf("retention unknown for %s: %w", key(r.ObjectRef), err)
		}
		if !allowed || historyReferenced(r, g) {
			continue
		}
		eligible[key(r.ObjectRef)] = r.ObjectRef
		review.plan.Candidates = append(review.plan.Candidates, r.ObjectRef)
	}
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
func historyEligible(r Resource, target client.Object, policy HistoryPolicy, now time.Time, evaluate HistoryEvaluator) (bool, error) {
	if !isHistory(r) || !r.Terminal || r.CompletedAt == nil || !older(*r.CompletedAt, now, policy.HistoryFor) || r.UID == "" || r.ResourceVersion == "" || r.DeletingAt != nil || len(r.Finalizers) > 0 || r.AttachedClients > 0 {
		return false, nil
	}
	if evaluate == nil {
		return false, fmt.Errorf("canonical history evaluator unavailable")
	}
	return evaluate(r.object, target, now)
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
		if !isHistory(Resource{ObjectRef: ref}) || ref.Namespace == "" || ref.Name == "" || ref.UID == "" || ref.ResourceVersion == "" || (plan.Namespace != "" && ref.Namespace != plan.Namespace) || seen[key(ref)] {
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
