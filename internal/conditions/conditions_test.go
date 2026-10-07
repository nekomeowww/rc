package conditions

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReadyAtGeneration(t *testing.T) {
	t.Parallel()
	ready := func(status metav1.ConditionStatus, generation int64) []metav1.Condition {
		return []metav1.Condition{{Type: "Ready", Status: status, ObservedGeneration: generation}}
	}
	for _, scenario := range []struct {
		name     string
		conds    []metav1.Condition
		observed int64
		want     bool
	}{
		{name: "current", conds: ready(metav1.ConditionTrue, 2), observed: 2, want: true},
		{name: "newer", conds: ready(metav1.ConditionTrue, 3), observed: 3, want: true},
		{name: "missing", observed: 2},
		{name: "false", conds: ready(metav1.ConditionFalse, 2), observed: 2},
		{name: "stale condition", conds: ready(metav1.ConditionTrue, 1), observed: 2},
		{name: "stale status", conds: ready(metav1.ConditionTrue, 2), observed: 1},
	} {
		if got := ReadyAtGeneration(scenario.conds, "Ready", 2, scenario.observed); got != scenario.want {
			t.Errorf("%s: ReadyAtGeneration() = %v, want %v", scenario.name, got, scenario.want)
		}
	}
}
