package maintenance

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nekomeowww/rc/internal/audit"
	clioutput "github.com/nekomeowww/rc/pkg/output"
)

const resourceColumn = "RESOURCE"

func objectName(ref audit.ObjectRef) string { return ref.Kind + "/" + ref.Namespace + "/" + ref.Name }

func capacity(bytes *int64) string {
	if bytes == nil {
		return "unknown"
	}
	return resource.NewQuantity(*bytes, resource.BinarySI).String()
}

func evidence(r audit.Resource) string {
	parts := []string{"uid=" + string(r.UID), "rv=" + r.ResourceVersion}
	for _, owner := range r.Owners {
		parts = append(parts, "owner="+owner.Kind+"/"+owner.Name+" uid="+string(owner.UID))
	}
	if r.DeletingAt != nil {
		parts = append(parts, "deleting="+r.DeletingAt.String())
	}
	if len(r.Finalizers) > 0 {
		parts = append(parts, "finalizers="+strings.Join(r.Finalizers, ","))
	}
	if r.Holder != "" {
		parts = append(parts, "holder="+r.Holder)
	}
	if r.Reservation != "" {
		parts = append(parts, "reservation="+r.Reservation)
	}
	if r.CapacityBytes != nil {
		parts = append(parts, "bound-capacity="+capacity(r.CapacityBytes))
	}
	for _, ref := range r.References {
		parts = append(parts, ref.Relation+"="+objectName(ref.Target))
	}
	return strings.Join(parts, "\n")
}

func reportTable(report audit.Report) clioutput.Table {
	table := clioutput.Table{Columns: []clioutput.Column{{Name: resourceColumn}, {Name: "STATE"}, {Name: "REQUESTED"}, {Name: "EVIDENCE"}, {Name: "FINDINGS"}}}
	scope := report.Inventory.Namespace
	if scope == "" {
		scope = "all namespaces"
	}
	table.Rows = append(table.Rows, []any{"Inventory", scope, "backend usage unknown", fmt.Sprintf("complete=%t; resources=%d; terminal-history=%d; visible-PVC-requests=%s; unknown-PVC-requests=%d", report.Inventory.Complete, report.Summary.Resources, report.Summary.TerminalRecords, capacity(&report.Summary.PVCRequestedBytes), report.Summary.UnknownPVCRequests), report.Inventory.ObservedAt.String()})
	for _, observation := range report.Inventory.Coverage {
		if !observation.Complete {
			table.Rows = append(table.Rows, []any{observation.Kind, "unknown", "-", observation.Error, "VisibilityUnknown"})
		}
	}
	for _, r := range report.Inventory.Resources {
		messages := []string{}
		for _, finding := range report.Findings {
			if finding.Resource == r.ObjectRef {
				messages = append(messages, finding.Code+": "+finding.Message)
			}
		}
		requested := "-"
		if r.Kind == "PersistentVolumeClaim" {
			requested = capacity(r.RequestedBytes)
		}
		table.Rows = append(table.Rows, []any{objectName(r.ObjectRef), r.Phase, requested, evidence(r), strings.Join(messages, "\n")})
	}
	return table
}

func planTable(plan audit.CleanupPlan) clioutput.Table {
	table := clioutput.Table{Columns: []clioutput.Column{{Name: resourceColumn}, {Name: "UID"}, {Name: "VERSION"}}}
	table.Rows = append(table.Rows, []any{"Plan expires " + plan.ExpiresAt.String(), "history only", plan.Version})
	for _, ref := range plan.Candidates {
		table.Rows = append(table.Rows, []any{objectName(ref), string(ref.UID), ref.ResourceVersion})
	}
	return table
}

func resultTable(result audit.PruneResult) clioutput.Table {
	table := clioutput.Table{Columns: []clioutput.Column{{Name: resourceColumn}, {Name: "RESULT"}}}
	for _, ref := range result.Requested {
		table.Rows = append(table.Rows, []any{objectName(ref), "deletion requested"})
	}
	for _, ref := range result.Absent {
		table.Rows = append(table.Rows, []any{objectName(ref), "observed absent"})
	}
	for _, ref := range result.Pending {
		table.Rows = append(table.Rows, []any{objectName(ref), "deletion pending; run doctor to inspect convergence"})
	}
	if len(table.Rows) == 0 {
		table.Rows = append(table.Rows, []any{"-", "no deletion requested"})
	}
	return table
}
