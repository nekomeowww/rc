package audit

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	severityUnknown    = "unknown"
	missingRuntimeCode = "MissingRuntime"
	missingStorageCode = "MissingStorage"
)

func diagnose(inventory Inventory, policy Policy) []Finding {
	findings := []Finding{}
	for _, observation := range inventory.Coverage {
		if !observation.Complete {
			findings = append(findings, Finding{Code: "VisibilityUnknown", Severity: severityUnknown, Resource: ObjectRef{APIVersion: observation.APIVersion, Kind: observation.Kind, Namespace: inventory.Namespace}, Message: observation.Error})
		}
	}
	g := newGraph(inventory)
	managed := g.managed()
	for _, r := range inventory.Resources {
		if !managed[key(r.ObjectRef)] && r.Kind != pvcKind {
			continue
		}
		findings = append(findings, resourceFindings(r, g, inventory, policy)...)
	}
	return findings
}

func resourceFindings(r Resource, g graph, inventory Inventory, policy Policy) []Finding {
	findings := []Finding{}
	add := func(code, severity, message string, related ...ObjectRef) {
		findings = append(findings, Finding{Code: code, Severity: severity, Resource: r.ObjectRef, Message: message, Related: related})
	}
	if r.DeletingAt != nil {
		related := make([]ObjectRef, 0, len(r.References))
		for _, ref := range g.incoming(r) {
			related = append(related, ref.Target)
		}
		leases := g.leases(r)
		for _, lease := range leases {
			related = append(related, lease.ObjectRef)
		}
		code := "DeletionPending"
		if older(*r.DeletingAt, inventory.ObservedAt.Time, policy.UnhealthyFor) {
			code = "DeletionBlocker"
		}
		var details strings.Builder
		details.WriteString("Deletion has not converged; finalizers=" + strings.Join(r.Finalizers, ","))
		for _, lease := range leases {
			details.WriteString("; Lease=" + lease.Name + " holder=" + lease.Holder + " reservation=" + lease.Reservation)
		}
		add(code, "warning", details.String(), related...)
	}
	if isRC(r) {
		if r.ObservedGeneration < r.Generation {
			add("StaleStatus", severityUnknown, "Status does not reflect the current spec generation")
		}
		for _, c := range r.Conditions {
			if c.Type == "Ready" || c.Type == "StorageReady" || c.Type == "VolumeReady" {
				if c.Status != metav1.ConditionTrue && older(c.LastTransitionTime, inventory.ObservedAt.Time, policy.UnhealthyFor) {
					add("LongUnhealthy", "warning", c.Type+"="+string(c.Status)+": "+c.Reason+"; "+c.Message)
				}
			}
		}
		findings = append(findings, dependencyFindings(r, g, inventory)...)
	}
	if r.Kind == worktreeKind && inventory.Complete && r.DeletingAt == nil && len(g.worktreeBlockers(r, inventory, policy)) == 0 {
		add("UnreferencedWorktree", "warning", "No observed mounts, active/recent executions, live PVC consumers or Leases; Git data safety remains unknown")
	}
	if r.Kind == podKind && r.Terminal && runtimePod(r, g) {
		add("TerminalRuntimePod", "warning", "Runtime Pod is "+r.Phase+": "+r.Reason+"; "+r.Message)
	}
	pvcFindings(r, g, policy, add)
	if isHistory(r) && r.Terminal {
		add("TerminalHistory", "info", "Terminal "+r.Phase+" record; retention and dependencies determine cleanup eligibility")
	}
	return findings
}

// dependencyFindings distinguishes absent dependencies from unreadable evidence.
// A stale Ready condition cannot establish that a referenced Pod or PVC exists.
func dependencyFindings(r Resource, g graph, inventory Inventory) []Finding {
	result := []Finding{}
	for _, ref := range r.References {
		if ref.Relation != pvcRelation && ref.Relation != runtimeRelation {
			continue
		}
		target, exists := g.resolve(ref.Target)
		if !exists {
			result = append(result, unresolvedDependency(r, ref, inventory))
			continue
		}
		if ref.Relation == pvcRelation && !controlledBy(target, r) {
			result = append(result, Finding{Code: "PVCConflict", Severity: severityUnknown, Resource: r.ObjectRef, Message: "Expected PVC is not controlled by this resource UID; legacy/missing owners are unverified", Related: []ObjectRef{target.ObjectRef}})
		}
	}
	return result
}

func unresolvedDependency(r Resource, ref Reference, inventory Inventory) Finding {
	code, noun := missingStorageCode, "storage PVC"
	unknownCode := "StorageUnknown"
	if ref.Relation == runtimeRelation {
		code, noun, unknownCode = missingRuntimeCode, "runtime Pod", "RuntimeUnknown"
	}
	finding := Finding{Code: code, Severity: "warning", Resource: r.ObjectRef, Related: []ObjectRef{ref.Target}, Message: "Referenced " + noun + " " + ref.Target.Namespace + "/" + ref.Target.Name + " is absent from the successfully read inventory; Ready status may be stale"}
	if !inventory.covers(ref.Target) {
		finding.Code, finding.Severity = unknownCode, severityUnknown
		finding.Message = "Cannot determine whether referenced " + noun + " " + ref.Target.Namespace + "/" + ref.Target.Name + " exists: target kind or namespace was not fully observed"
	}
	return finding
}

// covers requires positive evidence that this exact kind and namespace were
// listed. Overall partial visibility does not invalidate successful other lists.
func (inventory Inventory) covers(ref ObjectRef) bool {
	if inventory.Namespace != "" && inventory.Namespace != ref.Namespace {
		return false
	}
	for _, observation := range inventory.Coverage {
		if observation.APIVersion == ref.APIVersion && observation.Kind == ref.Kind {
			return observation.Complete
		}
	}
	return false
}

func runtimePod(pod Resource, g graph) bool {
	for _, incoming := range g.incoming(pod) {
		if incoming.Relation == "referenced-by:"+runtimeRelation {
			return true
		}
	}
	for _, ref := range pod.References {
		if ref.Relation == ownerRelation && (ref.Target.Kind == workspaceKind || ref.Target.Kind == "WorkspaceEnvironment") {
			if _, ok := g.resolve(ref.Target); ok {
				return true
			}
		}
	}
	return false
}

// pvcFindings reports PVC evidence through the caller's add, keeping one
// Finding constructor per resource.
func pvcFindings(r Resource, g graph, policy Policy, add func(code, severity, message string, related ...ObjectRef)) {
	if r.Kind != pvcKind {
		return
	}
	if len(r.Owners) == 0 {
		add("UnverifiedPVCOwner", severityUnknown, "PVC has no owner reference; provenance and data lifetime cannot be inferred")
	}
	if r.Phase == "Pending" {
		add("PendingPVC", "warning", "PVC is Pending; requested capacity is not provisioned backend usage")
		for _, incoming := range g.incoming(r) {
			event, _ := g.resolve(incoming.Target)
			if event.Kind == "Event" && event.Reason == "ProvisioningFailed" {
				add("ProvisioningFailure", "warning", "Pending PVC: "+event.Message, event.ObjectRef)
			}
		}
	}
	if r.RequestedBytes != nil && *r.RequestedBytes >= policy.LargePVCBytes {
		add("LargeStorageRequest", "info", fmt.Sprintf("PVC requests %d bytes; actual backend usage is unknown", *r.RequestedBytes))
	}
}
