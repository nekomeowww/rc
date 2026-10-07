package audit

import (
	"encoding/json"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

type graph struct {
	resources    []Resource
	byKey        map[string]Resource
	incomingRefs map[string][]Reference
	adjacent     map[string][]string
}

// newGraph indexes UID-qualified references once so large execution histories
// do not require repeated full-inventory walks for every candidate.
func newGraph(inventory Inventory) graph {
	g := graph{resources: inventory.Resources, byKey: make(map[string]Resource, len(inventory.Resources)), incomingRefs: map[string][]Reference{}, adjacent: map[string][]string{}}
	for _, r := range inventory.Resources {
		g.byKey[key(r.ObjectRef)] = r
	}
	for _, r := range inventory.Resources {
		for _, ref := range r.References {
			if _, exists := g.resolve(ref.Target); !exists {
				continue
			}
			source, target := key(r.ObjectRef), key(ref.Target)
			g.incomingRefs[target] = append(g.incomingRefs[target], Reference{Target: r.ObjectRef, Relation: "referenced-by:" + ref.Relation})
			g.adjacent[source] = append(g.adjacent[source], target)
			g.adjacent[target] = append(g.adjacent[target], source)
		}
	}
	return g
}

func (g graph) resolve(ref ObjectRef) (Resource, bool) {
	r, ok := g.byKey[key(ref)]
	return r, ok && matches(ref, r.ObjectRef)
}

func isRC(r Resource) bool { return r.APIVersion == repoAPI || r.APIVersion == workspaceAPI }

func isHistory(r Resource) bool {
	return (r.APIVersion == workspaceAPI && r.Kind == workspaceExecKind) || (r.APIVersion == repoAPI && slices.Contains([]string{"WorktreeExec", "RepositoryExec", "RepositorySync"}, r.Kind))
}

func older(timestamp metav1.Time, now time.Time, age time.Duration) bool {
	return !timestamp.IsZero() && !timestamp.After(now) && now.Sub(timestamp.Time) >= age
}

func recentlyActive(r Resource, now time.Time, age time.Duration) bool {
	return !r.Terminal || r.CompletedAt == nil || !older(*r.CompletedAt, now, age)
}

func (g graph) incoming(target Resource) []Reference { return g.incomingRefs[key(target.ObjectRef)] }

// pvcs resolves direct storage references and UID-proven owned claims for
// doctor. It does not construct a predicted deletion cascade or reclaim estimate.
func (g graph) pvcs(root Resource) []Resource {
	result := []Resource{}
	for _, r := range g.resources {
		if r.Kind != pvcKind {
			continue
		}
		referenced := controlledBy(r, root)
		for _, ref := range root.References {
			if ref.Relation == pvcRelation && matches(ref.Target, r.ObjectRef) {
				referenced = true
			}
		}
		if referenced {
			result = append(result, r)
		}
	}
	return result
}

func (g graph) leases(root Resource) []Resource {
	result := []Resource{}
	for _, r := range g.resources {
		if r.Kind != leaseKind || r.Namespace != root.Namespace {
			continue
		}
		if leaseReferences(r, root) {
			result = append(result, r)
		}
	}
	return result
}

func leaseReferences(lease, root Resource) bool {
	for _, ref := range lease.References {
		if matches(ref.Target, root.ObjectRef) {
			return true
		}
	}
	if root.UID != "" && (lease.Holder == string(root.UID) || lease.Holder == "worktree-delete/"+string(root.UID)) {
		return true
	}
	if root.Kind == worktreeKind {
		worktree := &repositories.Worktree{ObjectMeta: metav1.ObjectMeta{Name: root.Name, Namespace: root.Namespace, UID: root.UID}}
		if lease.Name == worktreeclaim.LeaseName(worktree) {
			return true
		}
	}
	if lease.Reservation == "" {
		return false
	}
	var reservation struct {
		Holders map[string]bool `json:"holders"`
	}
	if err := json.Unmarshal([]byte(lease.Reservation), &reservation); err != nil {
		return true
	}
	return reservation.Holders[root.Kind+"/"+string(root.UID)+"/"+root.Name]
}

func (g graph) worktreeBlockers(r Resource, inventory Inventory, policy Policy) []string {
	blockers := []string{}
	if r.Locked {
		blockers = append(blockers, "Worktree lock intent is set")
	}
	if !older(r.CreatedAt, inventory.ObservedAt.Time, policy.UnusedFor) {
		blockers = append(blockers, "Worktree age is recent or unknown")
	}
	for _, incoming := range g.incoming(r) {
		source, _ := g.resolve(incoming.Target)
		if source.Kind == workspaceKind && incoming.Relation == "referenced-by:mount" {
			blockers = append(blockers, "Workspace mount: "+source.Namespace+"/"+source.Name)
		}
		if source.Kind == "WorktreeExec" && recentlyActive(source, inventory.ObservedAt.Time, policy.UnusedFor) {
			blockers = append(blockers, "Active, recent or undated WorktreeExec: "+source.Name)
		}
	}
	for _, pvc := range g.pvcs(r) {
		for _, incoming := range g.incoming(pvc) {
			source, _ := g.resolve(incoming.Target)
			if (source.Kind == podKind || source.Kind == jobKind) && !source.Terminal {
				blockers = append(blockers, "Live PVC consumer: "+source.Kind+"/"+source.Name)
			}
		}
	}
	if len(g.leases(r)) > 0 {
		blockers = append(blockers, "Worktree Lease exists; age does not establish release")
	}
	return blockers
}

func (g graph) managed() map[string]bool {
	result := map[string]bool{}
	queue := make([]string, 0, len(g.resources))
	for _, r := range g.resources {
		if isRC(r) {
			result[key(r.ObjectRef)] = true
			queue = append(queue, key(r.ObjectRef))
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, neighbor := range g.adjacent[queue[i]] {
			if result[neighbor] {
				continue
			}
			result[neighbor] = true
			queue = append(queue, neighbor)
		}
	}
	return result
}

func controlledBy(pvc, owner Resource) bool {
	for _, ref := range pvc.Owners {
		if ref.Controller != nil && *ref.Controller && ref.UID != "" && ref.UID == owner.UID && ref.Name == owner.Name && ref.Kind == owner.Kind && ref.APIVersion == owner.APIVersion {
			return true
		}
	}
	return false
}
