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

func historyTable(report audit.HistoryReport) clioutput.Table {
	table := clioutput.Table{Columns: []clioutput.Column{{Name: "TARGET"}, {Name: "RETENTION"}, {Name: "RETAINED"}, {Name: "PENDING-CLEANUP"}, {Name: "DELETING"}, {Name: "POLICY", Wide: true}}}
	for _, observation := range report.Coverage {
		if !observation.Complete {
			table.Rows = append(table.Rows, []any{observation.Kind, audit.RetentionUnknown, "-", "-", "-", observation.Error})
		}
	}
	for _, target := range report.Targets {
		table.Rows = append(table.Rows, []any{objectName(target.Target), target.Retention, target.Retained, target.PendingCleanup, target.Deleting, retentionPolicy(target)})
	}
	if len(table.Rows) == 0 {
		table.Rows = append(table.Rows, []any{"-", "no execution targets", 0, 0, 0, "-"})
	}
	return table
}

// retentionPolicy shows the target's spec values; omitted fields use the
// executionretention defaults that the controller applies.
func retentionPolicy(target audit.HistoryTarget) string {
	if target.Policy == nil {
		return "-"
	}
	ttl, entries := "default", "default"
	if target.Policy.TTLAfterFinished != nil {
		ttl = target.Policy.TTLAfterFinished.Duration.String()
	}
	if target.Policy.MaxEntries > 0 {
		entries = fmt.Sprint(target.Policy.MaxEntries)
	}
	return "ttlAfterFinished=" + ttl + " maxEntries=" + entries
}
