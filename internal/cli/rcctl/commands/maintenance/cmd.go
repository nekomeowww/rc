// Package maintenance adapts audit results and reviewed plans to rcctl.
package maintenance

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nekomeowww/rc/internal/audit"
	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/executionretention"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	clioutput "github.com/nekomeowww/rc/pkg/output"
)

const fromPlanFlag = "from-plan"
const doctorCommand = "doctor"

type scanOptions struct {
	policy        audit.Policy
	allNamespaces bool
	largePVC      string
	output        clioutput.Options
}

type pruneOptions struct {
	allNamespaces bool
	historyFor    time.Duration
	output        clioutput.Options
	dryRun        bool
	yes           bool
	fromPlan      string
}

// Register attaches read-only doctor and previewable, confirmed history pruning.
func Register(root *cobra.Command, flags *kubeconfig.Flags) {
	root.AddCommand(newDoctorCommand(flags), newPruneCommand(flags, executionretention.BuildForTarget))
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
	cmd.Flags().DurationVar(&options.policy.HistoryFor, "history-for", options.policy.HistoryFor, "Minimum age since terminal execution completion")
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

func newPruneCommand(flags *kubeconfig.Flags, evaluate audit.HistoryEvaluator) *cobra.Command {
	options := &pruneOptions{historyFor: audit.DefaultPolicy().HistoryFor}
	cmd := &cobra.Command{
		Use: "prune", Short: "Preview or prune old terminal execution history",
		Long:    "Scan and show a cleanup plan, then request confirmation before deleting eligible terminal execution history. Use --dry-run for a read-only preview, --from-plan to reuse a saved JSON plan, or --yes to skip confirmation. Use doctor for Workspace, Worktree, Repository and PVC diagnostics. Every prune revalidates references and UID/resourceVersion; saved plans expire after one hour.",
		Example: "  rcctl prune --dry-run\n  rcctl prune --dry-run -o json > plan.json\n  rcctl prune\n  rcctl prune --from-plan plan.json --yes",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runPrune(cmd, flags, options, evaluate) },
	}
	cmd.Flags().BoolVarP(&options.allNamespaces, "all-namespaces", "A", false, "Read all namespaces; missing permissions remain unknown")
	cmd.Flags().DurationVar(&options.historyFor, "history-for", options.historyFor, "Additional minimum history age; never overrides retain or target policy")
	options.output.AddFlags(cmd, true)
	cmd.Flags().BoolVar(&options.dryRun, "dry-run", false, "Print the cleanup plan without modifying the cluster")
	cmd.Flags().StringVar(&options.fromPlan, fromPlanFlag, "", "Use a reviewed JSON plan; revalidate its scope, expiry and evidence")
	cmd.Flags().BoolVar(&options.yes, "yes", false, "Skip confirmation for eligible terminal-history deletions; --dry-run remains read-only")
	return cmd
}

// runPrune prepares a concrete preview before confirmation. Prune performs its
// per-record revalidation after confirmation, including time spent waiting for input.
func runPrune(cmd *cobra.Command, flags *kubeconfig.Flags, options *pruneOptions, evaluate audit.HistoryEvaluator) error {
	if err := options.output.Validate(true); err != nil {
		return err
	}
	connection, review, err := preparePrune(cmd, flags, options, evaluate)
	if err != nil {
		return err
	}
	plan := review.Plan()
	for _, unknown := range review.Unknowns {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UNKNOWN: "+unknown); err != nil {
			return err
		}
	}
	if options.dryRun {
		return options.output.PrintValue(cmd.OutOrStdout(), plan, planTable(plan))
	}
	if err := confirmPrune(cmd, plan, options.yes); err != nil {
		return err
	}
	result, pruneErr := audit.Prune(cmd.Context(), connection.Kube, review, time.Now())
	if err := options.output.PrintValue(cmd.OutOrStdout(), result, resultTable(result)); err != nil {
		return err
	}
	return pruneErr
}

func preparePrune(cmd *cobra.Command, flags *kubeconfig.Flags, options *pruneOptions, evaluate audit.HistoryEvaluator) (*cluster.Client, audit.ReviewedPlan, error) {
	var saved *audit.CleanupPlan
	empty := audit.ReviewedPlan{}
	if cmd.Flags().Changed(fromPlanFlag) {
		if options.fromPlan == "" {
			return nil, empty, fmt.Errorf("--from-plan requires a JSON plan file")
		}
		if cmd.Flags().Changed("history-for") {
			return nil, empty, fmt.Errorf("--history-for cannot override --from-plan; generate a new dry-run plan")
		}
		plan, err := readPlan(options.fromPlan)
		if err != nil {
			return nil, empty, err
		}
		saved = &plan
	}
	connection, namespace, err := connect(flags, options.allNamespaces)
	if err != nil {
		return nil, empty, err
	}
	policy := audit.HistoryPolicy{HistoryFor: options.historyFor}
	if saved != nil {
		if (cmd.Flags().Changed("namespace") || options.allNamespaces) && namespace != saved.Namespace {
			return nil, empty, fmt.Errorf("requested namespace differs from the reviewed plan scope")
		}
		namespace, policy = saved.Namespace, saved.Policy
	}
	review, err := audit.Review(cmd.Context(), connection.Kube, namespace, policy, saved, evaluate, time.Now())
	return connection, review, err
}

// confirmPrune keeps the preview and prompt on stderr so structured stdout is
// one result document. Only an explicit "yes" line permits writes; EOF declines.
func confirmPrune(cmd *cobra.Command, plan audit.CleanupPlan, yes bool) error {
	count := len(plan.Candidates)
	if yes || count == 0 {
		return nil
	}
	if err := (clioutput.Options{}).PrintValue(cmd.ErrOrStderr(), plan, planTable(plan)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Prune %d terminal history records? Type yes to continue: ", count); err != nil {
		return err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil || strings.TrimSpace(answer) != "yes" {
		return fmt.Errorf("prune cancelled; no deletions requested (use --yes for noninteractive execution)")
	}
	return nil
}

// readPlan rejects unknown fields and trailing documents before cluster access.
// Plan files contain executable identities only; they never carry credentials.
func readPlan(path string) (audit.CleanupPlan, error) {
	file, err := os.Open(path)
	if err != nil {
		return audit.CleanupPlan{}, fmt.Errorf("open cleanup plan: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var plan audit.CleanupPlan
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("decode cleanup plan: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return plan, fmt.Errorf("cleanup plan must contain exactly one JSON document")
	}
	return plan, nil
}
