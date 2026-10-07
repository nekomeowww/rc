// Package conditions holds status condition predicates shared by rc kinds.
package conditions

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReadyAtGeneration reports whether condType is True and both the condition
// and the status were observed at or after generation. A stale True condition
// from an earlier spec does not count.
func ReadyAtGeneration(conds []metav1.Condition, condType string, generation, observedGeneration int64) bool {
	if observedGeneration < generation {
		return false
	}
	condition := meta.FindStatusCondition(conds, condType)
	return condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration >= generation
}
