package audit

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	severityUnknown    = "unknown"
	missingRuntimeCode = "MissingRuntime"
	missingStorageCode = "MissingStorage"

	readyCondition           = "Ready"
	deletionBlockedCondition = "DeletionBlocked"
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
		for _, ref := range r.References {
			if ref.Relation == holderRelation {
				related = append(related, ref.Target)
			}
		}
		code, details := deletionEvidence(r, inventory.ObservedAt.Time, policy)
		if len(r.Holders) > 0 {
			details.WriteString("; holders=" + holderList(r.Holders))
		}
		add(code, "warning", details.String(), related...)
	}
	if isRC(r) {
		if r.ObservedGeneration < r.Generation {
			add("StaleStatus", severityUnknown, "Status does not reflect the current spec generation")
		}
		for _, c := range r.Conditions {
			if c.Type == readyCondition || c.Type == "StorageReady" || c.Type == "VolumeReady" {
				if c.Status != metav1.ConditionTrue && older(c.LastTransitionTime, inventory.ObservedAt.Time, policy.UnhealthyFor) {
					add("LongUnhealthy", "warning", c.Type+"="+string(c.Status)+": "+c.Reason+"; "+c.Message)
				}
			}
		}
		findings = append(findings, dependencyFindings(r, g, inventory)...)
	}
	if r.Kind == worktreeKind && inventory.Complete && r.DeletingAt == nil && len(g.worktreeBlockers(r, inventory, policy)) == 0 {
		add("UnreferencedWorktree", "warning", "No observed mounts, active/recent executions, live PVC consumers or holders; Git data safety remains unknown")
	}
	pvcFindings(r, g, policy, add)
	if isHistory(r) && r.Terminal {
		add("TerminalHistory", "info", "Terminal "+r.Phase+" record; retention and dependencies determine cleanup eligibility")
	}
	return findings
}

// deletionEvidence explains a pending deletion. Kinds whose controller
// publishes DeletionBlocked report its reason, message and age; other kinds,
// and publishing kinds whose controller has not yet written the condition, fall
// back to the finalizers that hold the object.
func deletionEvidence(r Resource, now time.Time, policy Policy) (string, *strings.Builder) {
	details := &strings.Builder{}
	since := *r.DeletingAt
	blocked := meta.FindStatusCondition(r.Conditions, deletionBlockedCondition)
	switch {
	case publishesDeletionBlocked(r) && blocked != nil && blocked.Status == metav1.ConditionTrue:
		since = blocked.LastTransitionTime
		details.WriteString("DeletionBlocked=" + blocked.Reason + " for " + age(since, now) + ": " + blocked.Message)
	case publishesDeletionBlocked(r):
		details.WriteString("Deletion has not converged; DeletionBlocked is not published; finalizers=" + strings.Join(r.Finalizers, ","))
	default:
		details.WriteString("Deletion has not converged; finalizers=" + strings.Join(r.Finalizers, ","))
	}
	if older(since, now, policy.UnhealthyFor) {
		return "DeletionBlocker", details
	}
	return "DeletionPending", details
}

// publishesDeletionBlocked lists the kinds whose finalizer-holding (or, for
// Repository, garbage-collection-observing) controller writes DeletionBlocked.
func publishesDeletionBlocked(r Resource) bool {
	return (r.APIVersion == workspaceAPI && r.Kind == workspaceKind) ||
		(r.APIVersion == repoAPI && (r.Kind == worktreeKind || r.Kind == repositoryKind))
}

func age(since metav1.Time, now time.Time) string {
	if since.IsZero() || since.After(now) {
		return "unknown time"
	}
	return now.Sub(since.Time).Round(time.Second).String()
}

// dependencyFindings reports storage and runtime failures that the controller
// published in StorageReady/VolumeReady and Ready. It keeps one direct
// existence check of the PVC and Pod named in status: a stale Ready condition
// from a controller that is down cannot establish that they exist.
func dependencyFindings(r Resource, g graph, inventory Inventory) []Finding {
	result := []Finding{}
	// missing records relations the controller already reports as absent, so
	// the existence check does not repeat them.
	missing := map[string]bool{}
	add := func(code, relation, message string) {
		related := []ObjectRef{}
		for _, ref := range r.References {
			if ref.Relation == relation {
				related = append(related, ref.Target)
			}
		}
		missing[relation] = missing[relation] || code == missingStorageCode || code == missingRuntimeCode
		result = append(result, Finding{Code: code, Severity: "warning", Resource: r.ObjectRef, Message: message, Related: related})
	}
	if storage := storageCondition(r); storage != nil && storage.Status != metav1.ConditionTrue {
		switch storage.Reason {
		case workspaces.ReasonVolumeClaimLost:
			add(missingStorageCode, pvcRelation, storage.Type+"="+storage.Reason+": "+storage.Message)
		case workspaces.ReasonVolumeClaimConflict:
			add("PVCConflict", pvcRelation, storage.Type+"="+storage.Reason+": "+storage.Message)
		}
	}
	if ready := meta.FindStatusCondition(r.Conditions, readyCondition); r.Kind == workspaceKind && ready != nil && ready.Status != metav1.ConditionTrue {
		switch ready.Reason {
		case workspaces.WorkspaceReasonRuntimeMissing:
			add(missingRuntimeCode, runtimeRelation, "Ready="+ready.Reason+": "+ready.Message)
		case workspaces.WorkspaceReasonRuntimeTerminal:
			detail := ready.Reason
			if degraded := meta.FindStatusCondition(r.Conditions, workspaces.WorkspaceConditionDegraded); degraded != nil && degraded.Status == metav1.ConditionTrue {
				detail = degraded.Reason
			}
			add("TerminalRuntimePod", runtimeRelation, "Ready="+ready.Reason+" ("+detail+"): "+ready.Message)
		}
	}
	if r.StorageUnpublished && r.DeletingAt == nil && !missing[pvcRelation] {
		result = append(result, Finding{Code: "StorageUnpublished", Severity: severityUnknown, Resource: r.ObjectRef, Message: "Status names no PVC; storage is unknown until the controller publishes it"})
	}
	for _, ref := range r.References {
		if (ref.Relation != pvcRelation && ref.Relation != runtimeRelation) || missing[ref.Relation] {
			continue
		}
		if _, exists := g.resolve(ref.Target); !exists {
			result = append(result, unresolvedDependency(r, ref, inventory))
		}
	}
	return result
}

// storageCondition returns the condition in which the object's controller
// publishes PVC health: VolumeReady on Worktree, StorageReady elsewhere.
func storageCondition(r Resource) *metav1.Condition {
	if r.Kind == worktreeKind {
		return meta.FindStatusCondition(r.Conditions, repositories.WorktreeConditionVolumeReady)
	}
	return meta.FindStatusCondition(r.Conditions, workspaces.ConditionStorageReady)
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
