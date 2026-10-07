package audit

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Prune applies the pre-confirmation Review, without any further LISTs.
// Each selected record and its direct dependencies are GET again, then canonical
// eligibility is reevaluated. UID/resourceVersion fence the DELETE itself.
// Cross-object references cannot be atomically fenced; orphan propagation keeps
// late or unknown dependents and their data intact. Progress survives errors.
func Prune(ctx context.Context, kube client.Client, review ReviewedPlan, now time.Time) (PruneResult, error) {
	started := time.Now()
	result := PruneResult{Requested: []ObjectRef{}, Absent: []ObjectRef{}, Pending: []ObjectRef{}}
	plan := review.plan
	if err := validatePlan(plan, now); err != nil {
		return result, err
	}
	if len(review.Unknowns) > 0 || review.evaluate == nil {
		return result, fmt.Errorf("cannot prune with unknown eligibility: %v", review.Unknowns)
	}
	for _, ref := range plan.Candidates {
		object, err := getObject(ctx, kube, ref)
		if err != nil {
			return result, err
		}
		if object == nil {
			result.Absent = append(result.Absent, ref)
			continue
		}
		if object.GetUID() != ref.UID {
			return result, fmt.Errorf("stale cleanup plan: replaced %s", key(ref))
		}
		if object.GetDeletionTimestamp() != nil {
			result.Pending = append(result.Pending, ref)
			continue
		}
		if object.GetResourceVersion() != ref.ResourceVersion {
			return result, fmt.Errorf("stale cleanup plan: changed %s", key(ref))
		}
		r := project(object, ref.APIVersion, ref.Kind)
		target, err := getObject(ctx, kube, executionTarget(r))
		if err != nil {
			return result, err
		}
		for _, dependency := range r.References {
			if dependency.Relation != "execution-job" {
				continue
			}
			job, err := getObject(ctx, kube, dependency.Target)
			if err != nil {
				return result, err
			}
			if job != nil {
				return result, fmt.Errorf("execution Job still exists for %s", key(ref))
			}
		}
		currentTime := now.Add(time.Since(started))
		allowed, err := historyEligible(r, target, plan.Policy, currentTime, review.evaluate)
		if err != nil {
			return result, fmt.Errorf("retention unknown for %s: %w", key(ref), err)
		}
		if !allowed {
			return result, fmt.Errorf("history is no longer eligible: %s", key(ref))
		}
		if err := validatePlanTime(plan, now.Add(time.Since(started))); err != nil {
			return result, err
		}
		err = kube.Delete(ctx, object, client.Preconditions{UID: &ref.UID, ResourceVersion: &ref.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationOrphan))
		if apierrors.IsNotFound(err) {
			result.Absent = append(result.Absent, ref)
			continue
		}
		if err != nil {
			return result, fmt.Errorf("conditional delete %s: %w", key(ref), err)
		}
		result.Requested = append(result.Requested, ref)
		after, err := getObject(ctx, kube, ref)
		if err != nil {
			return result, err
		}
		if after == nil {
			result.Absent = append(result.Absent, ref)
			continue
		}
		if after.GetUID() != ref.UID {
			return result, fmt.Errorf("resource replaced after deletion: %s", key(ref))
		}
		result.Pending = append(result.Pending, ref)
	}
	return result, nil
}

// getObject reads a typed object for the canonical evaluator. Only NotFound
// establishes absence; RBAC and transport failures must never imply permission.
func getObject(ctx context.Context, kube client.Client, ref ObjectRef) (client.Object, error) {
	if ref.Name == "" {
		return nil, fmt.Errorf("execution target is missing")
	}
	object, err := kube.Scheme().New(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
	if err != nil {
		return nil, err
	}
	typed, ok := object.(client.Object)
	if !ok {
		return nil, fmt.Errorf("unsupported object %s", key(ref))
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, typed); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", key(ref), err)
	}
	return typed, nil
}
