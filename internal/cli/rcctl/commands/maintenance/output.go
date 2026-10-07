package maintenance

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	if r.Lifecycle != nil {
		parts = append(parts, fmt.Sprintf("active-executions=%d", r.Lifecycle.ActiveExecutions), "idle-suspend-at="+timestamp(r.Lifecycle.IdleSuspendAt), "delete-at="+timestamp(r.Lifecycle.DeleteAt))
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

// timestamp renders a published deadline; an empty deadline means no
// automatic action is scheduled.
func timestamp(t *metav1.Time) string {
	if t == nil {
		return "none"
	}
	return t.UTC().Format(time.RFC3339)
}

func historyTable(report audit.HistoryReport) clioutput.Table {
	table := clioutput.Table{Columns: []clioutput.Column{{Name: "TARGET"}, {Name: "RETENTION"}, {Name: "STATUS"}, {Name: "COMPLIANT"}, {Name: "RETAINED"}, {Name: "PENDING-CLEANUP"}, {Name: "POLICY", Wide: true}, {Name: "MESSAGE", Wide: true}}}
	for _, observation := range report.Coverage {
		if !observation.Complete {
			table.Rows = append(table.Rows, []any{observation.Kind, audit.RetentionUnknown, "-", "-", "-", "-", "-", observation.Error})
		}
	}
	for _, target := range report.Targets {
		table.Rows = append(table.Rows, []any{objectName(target.Target), target.Retention, target.Status, cmp.Or(target.Reason, "-"), count(target.Retained), count(target.PendingCleanup), effectivePolicy(target), cmp.Or(target.Message, "-")})
	}
	if len(table.Rows) == 0 {
		table.Rows = append(table.Rows, []any{"-", "no execution targets", "-", "-", "-", "-", "-", "-"})
	}
	return table
}

func count(value *int32) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprint(*value)
}

// effectivePolicy shows the defaulted policy the controller published.
func effectivePolicy(target audit.HistoryTarget) string {
	if target.EffectiveTTL == nil && target.EffectiveMaxEntries == 0 {
		return "-"
	}
	ttl := "-"
	if target.EffectiveTTL != nil {
		ttl = target.EffectiveTTL.Duration.String()
	}
	return fmt.Sprintf("ttlAfterFinished=%s maxEntries=%d", ttl, target.EffectiveMaxEntries)
}
