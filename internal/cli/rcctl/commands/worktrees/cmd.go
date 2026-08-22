package worktrees

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
)

type addOptions struct {
	repository   string
	name         string
	branch       string
	resetBranch  string
	ref          string
	detach       bool
	orphan       bool
	noCheckout   bool
	lock         bool
	lockReason   string
	storageClass string
	size         string
	accessModes  []string
	wait         bool
}

type worktreeListRow struct {
	name       string
	ready      bool
	repository string
	volume     string
	path       string
}

// Register attaches Worktree commands to the root command.
func Register(root *cobra.Command, kubeconfigFlags *kubeconfig.Flags) {
	worktreeCommand := NewCommand()
	worktreeCommand.AddCommand(newAddCommand(kubeconfigFlags))
	worktreeCommand.AddCommand(newListCommand(kubeconfigFlags))
	root.AddCommand(worktreeCommand)
}

func newListCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List Worktrees in the current namespace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, namespace, err := kubeconfigFlags.Resolve()
			if err != nil {
				return err
			}
			scheme := runtime.NewScheme()
			if err := repositoriesv1alpha1.AddToScheme(scheme); err != nil {
				return fmt.Errorf("register Repository API types: %w", err)
			}
			kubeClient, err := client.New(config, client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("create Kubernetes client: %w", err)
			}

			return runWorktreeList(cmd.Context(), cmd.OutOrStdout(), kubeClient, namespace)
		},
	}
}

func runWorktreeList(ctx context.Context, output io.Writer, kubeClient client.Client, namespace string) error {
	list := new(repositoriesv1alpha1.WorktreeList)
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list Worktrees: %w", err)
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tREADY\tREPOSITORY\tVOLUME\tPATH"); err != nil {
		return err
	}
	for _, row := range worktreeListRows(list.Items) {
		if _, err := fmt.Fprintf(writer, "%s\t%t\t%s\t%s\t%s\n", row.name, row.ready, row.repository, row.volume, row.path); err != nil {
			return err
		}
	}

	return writer.Flush()
}

func worktreeListRows(worktrees []repositoriesv1alpha1.Worktree) []worktreeListRow {
	rows := make([]worktreeListRow, 0, len(worktrees))
	for index := range worktrees {
		worktree := &worktrees[index]
		rows = append(rows, worktreeListRow{
			name: worktree.Name, ready: meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady),
			repository: worktreeValueOrDash(worktree.Spec.RepositoryRef.Name), volume: worktreeValueOrDash(worktree.Status.VolumeClaimName),
			path: worktreeValueOrDash(worktree.Status.WorktreePath),
		})
	}
	slices.SortFunc(rows, func(left worktreeListRow, right worktreeListRow) int {
		return strings.Compare(left.name, right.name)
	})

	return rows
}

func worktreeValueOrDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}

// NewCommand creates the Worktree command group.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "worktree",
		Aliases: []string{"worktrees"},
		Short:   "Manage independent Git worktrees",
		GroupID: command.WorktreesGroup,
	}
}

func newAddCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(addOptions)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create a child PVC and native Git worktree",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAdd(cmd, kubeconfigFlags, *options)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&options.repository, "repo", "r", "", "Repository name, host/path, or Git URL")
	flags.StringVar(&options.name, "name", "", "Worktree metadata.name; derive it from branch/ref when omitted")
	flags.StringVarP(&options.branch, "branch", "b", "", "Create a new local branch")
	flags.StringVarP(&options.resetBranch, "reset-branch", "B", "", "Create or reset a local branch")
	flags.StringVar(&options.ref, "ref", "", "Commit-ish to check out")
	flags.BoolVarP(&options.detach, "detach", "d", false, "Create a detached HEAD worktree")
	flags.BoolVar(&options.orphan, "orphan", false, "Create an unborn branch")
	flags.BoolVar(&options.noCheckout, "no-checkout", false, "Create worktree metadata without checking out files")
	flags.BoolVar(&options.lock, "lock", false, "Keep the new Git worktree locked")
	flags.StringVar(&options.lockReason, "reason", "", "Reason for locking the Git worktree")
	flags.StringVar(&options.storageClass, "storage-class", "", "Override the Repository StorageClass")
	flags.StringVar(&options.size, "size", "", "Override the child PVC size")
	flags.StringSliceVar(&options.accessModes, "access-mode", nil, "Override child PVC access modes (repeat or comma-separate)")
	flags.BoolVar(&options.wait, "wait", true, "Wait for the PVC clone and Git worktree bootstrap")
	_ = cmd.MarkFlagRequired("repo")
	return cmd
}

func runAdd(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, options addOptions) error {
	if options.name != "" {
		if errors := validation.IsDNS1123Subdomain(options.name); len(errors) > 0 {
			return fmt.Errorf("invalid Worktree name %q: %s", options.name, errors[0])
		}
	}

	config, namespace, err := kubeconfigFlags.Resolve()
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := repositoriesv1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register Repository API types: %w", err)
	}

	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	size, err := parseSize(options.size)
	if err != nil {
		return err
	}

	accessModes, err := parseAccessModes(options.accessModes)
	if err != nil {
		return err
	}

	worktreeClient := &repositoryservice.WorktreeClient{Client: kubeClient}
	worktree, err := worktreeClient.Start(cmd.Context(), repositoryservice.WorktreeAddRequest{
		Namespace:    namespace,
		Repository:   options.repository,
		Name:         options.name,
		Branch:       options.branch,
		ResetBranch:  options.resetBranch,
		Ref:          options.ref,
		Detach:       options.detach,
		Orphan:       options.orphan,
		NoCheckout:   options.noCheckout,
		Lock:         options.lock,
		LockReason:   options.lockReason,
		StorageClass: options.storageClass,
		Size:         size,
		AccessModes:  accessModes,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "worktree.repositories.rc.ayaka.io/%s created\n", worktree.Name); err != nil {
		return err
	}
	if !options.wait {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), worktree.Name)
		return err
	}
	if err := worktreeClient.Wait(cmd.Context(), worktree, cmd.OutOrStdout()); err != nil {
		return err
	}

	current := new(repositoriesv1alpha1.Worktree)
	if err := kubeClient.Get(cmd.Context(), client.ObjectKeyFromObject(worktree), current); err != nil {
		return fmt.Errorf("get ready Worktree: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "worktree/%s ready pvc/%s path=%s\n", current.Name, current.Status.VolumeClaimName, current.Status.WorktreePath)

	return err
}

func parseSize(value string) (*resource.Quantity, error) {
	if value == "" {
		return nil, nil
	}

	parsed, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("parse --size: %w", err)
	}
	if parsed.Sign() <= 0 {
		return nil, fmt.Errorf("--size must be greater than zero")
	}

	return &parsed, nil
}

func parseAccessModes(values []string) ([]corev1.PersistentVolumeAccessMode, error) {
	if len(values) == 0 {
		return nil, nil
	}

	accessModes := make([]corev1.PersistentVolumeAccessMode, 0, len(values))
	for _, value := range values {
		mode := corev1.PersistentVolumeAccessMode(value)
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadOnlyMany, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
			accessModes = append(accessModes, mode)
		default:
			return nil, fmt.Errorf("unsupported --access-mode %q", value)
		}
	}

	return accessModes, nil
}
