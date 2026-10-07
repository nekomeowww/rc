// Package maintenance adapts read-only audit reports to rcctl.
package maintenance

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nekomeowww/rc/internal/audit"
	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	clioutput "github.com/nekomeowww/rc/pkg/output"
)

const doctorCommand = "doctor"

type scanOptions struct {
	policy        audit.Policy
	allNamespaces bool
	largePVC      string
	output        clioutput.Options
}

type pruneOptions struct {
	allNamespaces bool
	output        clioutput.Options
}

// Register attaches the read-only doctor and prune reports.
func Register(root *cobra.Command, flags *kubeconfig.Flags) {
	root.AddCommand(newDoctorCommand(flags), newPruneCommand(flags))
}

func newDoctorCommand(flags *kubeconfig.Flags) *cobra.Command {
	options := &scanOptions{policy: audit.DefaultPolicy()}
	cmd := &cobra.Command{Use: doctorCommand, Short: "Audit rc references, storage, runtime health and deletion blockers (read-only)", Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := options.output.Validate(true); err != nil {
			return err
		}
		report, err := options.read(cmd, flags)
		if err != nil {
			return err
		}
		return options.output.PrintValue(cmd.OutOrStdout(), report, reportTable(report))
	}
	options.addFlags(cmd)
	return cmd
}

func (options *scanOptions) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&options.allNamespaces, "all-namespaces", "A", false, "Read all namespaces; missing permissions remain unknown")
	cmd.Flags().DurationVar(&options.policy.UnusedFor, "unused-for", options.policy.UnusedFor, "Minimum Worktree age and execution inactivity; not proof of data safety")
	cmd.Flags().DurationVar(&options.policy.UnhealthyFor, "unhealthy-for", options.policy.UnhealthyFor, "Minimum unhealthy condition duration")
	cmd.Flags().StringVar(&options.largePVC, "large-pvc", "100Gi", "Report PVC requests at or above this capacity (not actual usage)")
	options.output.AddFlags(cmd, true)
}

// read resolves the requested scope and obtains fresh evidence for doctor.
// Connection setup remains in the CLI; interpretation lives in audit.
func (options *scanOptions) read(cmd *cobra.Command, flags *kubeconfig.Flags) (audit.Report, error) {
	quantity, err := resource.ParseQuantity(options.largePVC)
	if err != nil || quantity.Sign() <= 0 {
		return audit.Report{}, fmt.Errorf("--large-pvc must be a positive Kubernetes capacity quantity")
	}
	options.policy.LargePVCBytes = quantity.Value()
	connection, namespace, err := connect(flags, options.allNamespaces)
	if err != nil {
		return audit.Report{}, err
	}
	return audit.Scan(cmd.Context(), connection.Kube, namespace, options.policy, time.Now())
}

// connect builds the cluster client and resolves the scan namespace; an empty
// namespace selects all namespaces.
func connect(flags *kubeconfig.Flags, allNamespaces bool) (*cluster.Client, string, error) {
	connection, namespace, err := cluster.Connect(flags)
	if err != nil {
		return nil, "", err
	}
	if allNamespaces {
		namespace = ""
	}
	return connection, namespace, nil
}

// pruneHint is printed on stderr so structured stdout stays one document.
const pruneHint = "rcctl prune is read-only and shows status.executionHistory as published by the rc controller. The controller deletes terminal execution history; configure it with spec.executionRetention (ttlAfterFinished, maxEntries) on the Workspace or WorkspaceEnvironment, or set spec.retain on a WorkspaceExec to keep it. STATUS Stale or Unpublished means the controller has not caught up or is not running."

func newPruneCommand(flags *kubeconfig.Flags) *cobra.Command {
	options := &pruneOptions{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Report terminal execution history awaiting controller cleanup (read-only)",
		Long: "Report, for each Workspace and WorkspaceEnvironment, the controller-published status.executionHistory: how many WorkspaceExec records are retained and how many wait for the controller to delete them, and the ExecutionHistoryCompliant reason. " +
			"Summaries older than the target spec are marked Stale and missing ones Unpublished; the command never recomputes them. " +
			"This command never deletes anything: the rc controller performs history cleanup with fresh reads and preconditions. To remove history sooner, lower spec.executionRetention.ttlAfterFinished or maxEntries on the target. Use doctor for Workspace, Worktree, Repository and PVC diagnostics.",
		Example: "  rcctl prune\n  rcctl prune -A -o json",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runPrune(cmd, flags, options) },
	}
	cmd.Flags().BoolVarP(&options.allNamespaces, "all-namespaces", "A", false, "Read all namespaces; missing permissions remain unknown")
	options.output.AddFlags(cmd, true)
	return cmd
}

// runPrune reads the published execution history backlog. It issues only LIST
// requests for Workspaces and WorkspaceEnvironments.
func runPrune(cmd *cobra.Command, flags *kubeconfig.Flags, options *pruneOptions) error {
	if err := options.output.Validate(true); err != nil {
		return err
	}
	connection, namespace, err := connect(flags, options.allNamespaces)
	if err != nil {
		return err
	}
	report, err := audit.ExecutionHistory(cmd.Context(), connection.Kube, namespace, time.Now())
	if err != nil {
		return err
	}
	for _, observation := range report.Coverage {
		if !observation.Complete {
			if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UNKNOWN: "+observation.Kind+" visibility unknown: "+observation.Error); err != nil {
				return err
			}
		}
	}
	if err := options.output.PrintValue(cmd.OutOrStdout(), report, historyTable(report)); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.ErrOrStderr(), pruneHint)
	return err
}
