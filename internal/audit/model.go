// Package audit correlates rc lifecycle intent with live Kubernetes evidence.
// Scan and Review are read-only; Prune only removes reviewed terminal records.
package audit

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Policy defines conservative age and capacity thresholds. Durations are encoded
// as nanoseconds in plan JSON, as with time.Duration; CLI flags accept 30m or 168h.
type Policy struct {
	UnusedFor     time.Duration `json:"unusedFor"`
	HistoryFor    time.Duration `json:"historyFor"`
	UnhealthyFor  time.Duration `json:"unhealthyFor"`
	LargePVCBytes int64         `json:"largePVCBytes"`
}

// DefaultPolicy retains recent activity for seven days and diagnoses unhealthy
// resources after thirty minutes. Storage thresholds refer to requests only.
func DefaultPolicy() Policy {
	return Policy{UnusedFor: 7 * 24 * time.Hour, HistoryFor: 7 * 24 * time.Hour, UnhealthyFor: 30 * time.Minute, LargePVCBytes: 100 * 1024 * 1024 * 1024}
}

// ObjectRef identifies a resource incarnation; resourceVersion is an opaque
// precondition, never a timestamp or a numerically comparable revision.
type ObjectRef struct {
	APIVersion      string    `json:"apiVersion"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid,omitempty"`
	ResourceVersion string    `json:"resourceVersion,omitempty"`
}

// Reference records why one resource uses another. UID is populated for owners
// and status identities; desired-state references may be name-only.
type Reference struct {
	Target   ObjectRef `json:"target"`
	Relation string    `json:"relation"`
}

// Resource is a credential-free projection of lifecycle evidence, not a raw CR.
// References are outgoing; Owners retain the original Kubernetes owner contract.
type Resource struct {
	// object preserves typed policy inputs for the canonical evaluator in memory only.
	object             client.Object
	ObjectRef          `json:",inline"`
	CreatedAt          metav1.Time             `json:"createdAt"`
	DeletingAt         *metav1.Time            `json:"deletingAt,omitempty"`
	Finalizers         []string                `json:"finalizers,omitempty"`
	Owners             []metav1.OwnerReference `json:"owners,omitempty"`
	References         []Reference             `json:"references,omitempty"`
	Conditions         []metav1.Condition      `json:"conditions,omitempty"`
	Generation         int64                   `json:"generation,omitempty"`
	ObservedGeneration int64                   `json:"observedGeneration,omitempty"`
	Phase              string                  `json:"phase,omitempty"`
	Reason             string                  `json:"reason,omitempty"`
	Message            string                  `json:"message,omitempty"`
	Terminal           bool                    `json:"terminal,omitempty"`
	CompletedAt        *metav1.Time            `json:"completedAt,omitempty"`
	Locked             bool                    `json:"locked,omitempty"`
	AttachedClients    int32                   `json:"attachedClients,omitempty"`
	RequestedBytes     *int64                  `json:"requestedBytes,omitempty"`
	CapacityBytes      *int64                  `json:"capacityBytes,omitempty"`
	StorageClass       string                  `json:"storageClass,omitempty"`
	Holder             string                  `json:"holder,omitempty"`
	Reservation        string                  `json:"reservation,omitempty"`
}

// Observation distinguishes an empty successful list from unavailable evidence.
type Observation struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Complete   bool   `json:"complete"`
	Error      string `json:"error,omitempty"`
}

// Inventory covers exactly Namespace (empty means all namespaces). It is a
// sequence of live lists, not a transactional cluster snapshot.
type Inventory struct {
	Namespace  string        `json:"namespace"`
	ObservedAt metav1.Time   `json:"observedAt"`
	Complete   bool          `json:"complete"`
	Coverage   []Observation `json:"coverage"`
	Resources  []Resource    `json:"resources"`
}

// Finding includes evidence and uncertainty without asserting a remediation is safe.
type Finding struct {
	Code     string      `json:"code"`
	Severity string      `json:"severity"`
	Resource ObjectRef   `json:"resource"`
	Message  string      `json:"message"`
	Related  []ObjectRef `json:"related,omitempty"`
}

// Report is shared by doctor tables and JSON; it is never embedded in a plan.
type Report struct {
	Inventory Inventory `json:"inventory"`
	Summary   Summary   `json:"summary"`
	Findings  []Finding `json:"findings"`
}

// HistoryPolicy adds a minimum age to canonical retention eligibility. It can
// only delay deletion; it never overrides retain intent or the target's policy.
type HistoryPolicy struct {
	HistoryFor time.Duration `json:"historyFor"`
}

// CleanupPlan stores only the reviewed executable history identities. It is an
// expiring selection, not a signed capability or a serialized inventory.
type CleanupPlan struct {
	Version    string        `json:"version"`
	Namespace  string        `json:"namespace"`
	Policy     HistoryPolicy `json:"policy"`
	CreatedAt  metav1.Time   `json:"createdAt"`
	ExpiresAt  metav1.Time   `json:"expiresAt"`
	Candidates []ObjectRef   `json:"candidates"`
}

// HistoryEvaluator is the integration boundary for T-663's canonical evaluator.
// The adapter must use that evaluator, not reimplement retention defaults,
// retain intent, target policy or completion rules. Inputs are typed, uncached
// objects; target is nil only after confirmed absence. Missing targets and
// unsupported kinds must fail closed unless the canonical policy permits them.
// It must be pure (no I/O), reject active/undated/attached records, and evaluate
// the effective target policy at now. False means retained; errors mean unknown.
// A nil evaluator disables pruning until the canonical implementation is wired.
type HistoryEvaluator func(record, target client.Object, now time.Time) (bool, error)

// ReviewedPlan can only be obtained through Review's fresh scan. Its private
// selection cannot be widened by editing a returned JSON plan after confirmation.
type ReviewedPlan struct {
	plan     CleanupPlan
	evaluate HistoryEvaluator
	// Unknowns explain why a preview cannot authorize deletion.
	Unknowns []string
}

// Plan returns a detached copy for previewing or saving the reviewed selection.
func (review ReviewedPlan) Plan() CleanupPlan {
	plan := review.plan
	plan.Candidates = append([]ObjectRef{}, plan.Candidates...)
	return plan
}

// PruneResult distinguishes accepted requests from observed absence. It is
// returned even on partial failure; an accepted DELETE is not proof of cleanup.
type PruneResult struct {
	Requested []ObjectRef `json:"requested"`
	Absent    []ObjectRef `json:"absent"`
	Pending   []ObjectRef `json:"pending"`
}

// Summary counts visible resources and PVC requests once per object, including
// resources whose relationship to rc cannot be established. Physical use is unknown.
type Summary struct {
	Resources          int   `json:"resources"`
	TerminalRecords    int   `json:"terminalRecords"`
	PVCRequestedBytes  int64 `json:"pvcRequestedBytes"`
	UnknownPVCRequests int   `json:"unknownPVCRequests"`
}

func summarize(inventory Inventory) Summary {
	summary := Summary{Resources: len(inventory.Resources)}
	for _, r := range inventory.Resources {
		if isHistory(r) && r.Terminal {
			summary.TerminalRecords++
		}
		if r.Kind != pvcKind {
			continue
		}
		if r.RequestedBytes == nil {
			summary.UnknownPVCRequests++
			continue
		}
		summary.PVCRequestedBytes += *r.RequestedBytes
	}
	return summary
}
