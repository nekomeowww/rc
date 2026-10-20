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

// Register attaches the read-only doctor report.
func Register(root *cobra.Command, flags *kubeconfig.Flags) {
	root.AddCommand(newDoctorCommand(flags))
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
