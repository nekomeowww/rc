package repositories

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

type repositoryListRow struct {
	name    string
	ready   bool
	remote  string
	ref     string
	volume  string
	updated string
}

// NewCommand creates the Repository command group.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "repo",
		Aliases: []string{"repository"},
		Short:   "Manage repositories",
		GroupID: command.RepositoriesGroup,
	}
}

func newListCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List Repositories in the current namespace", Args: cobra.NoArgs,
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

			return runRepositoryList(cmd.Context(), cmd.OutOrStdout(), kubeClient, namespace)
		},
	}
}

func newDeleteCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	return &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"remove", "rm"},
		Short:   "Delete a Repository and its owned storage and Jobs",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			return runRepositoryDelete(cmd.Context(), kubeClient, namespace, args[0])
		},
	}
}

func runRepositoryDelete(ctx context.Context, kubeClient client.Client, namespace string, name string) error {
	repository := &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if err := kubeClient.Delete(ctx, repository); err != nil {
		return fmt.Errorf("delete Repository: %w", err)
	}

	return nil
}

func runRepositoryList(ctx context.Context, output io.Writer, kubeClient client.Client, namespace string) error {
	list := new(repositoriesv1alpha1.RepositoryList)
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list Repositories: %w", err)
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tREADY\tREMOTE\tREF\tVOLUME\tUPDATED"); err != nil {
		return err
	}
	for _, row := range repositoryListRows(list.Items) {
		if _, err := fmt.Fprintf(writer, "%s\t%t\t%s\t%s\t%s\t%s\n", row.name, row.ready, row.remote, row.ref, row.volume, row.updated); err != nil {
			return err
		}
	}

	return writer.Flush()
}

func repositoryListRows(repositories []repositoriesv1alpha1.Repository) []repositoryListRow {
	rows := make([]repositoryListRow, 0, len(repositories))
	for index := range repositories {
		repository := &repositories[index]
		updated := "-"
		if repository.Status.LastUpdatedAt != nil {
			updated = repository.Status.LastUpdatedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, repositoryListRow{
			name: repository.Name, ready: meta.IsStatusConditionTrue(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady),
			remote: valueOrDash(repository.Spec.Remote.URL), ref: valueOrDash(repository.Spec.Ref),
			volume: valueOrDash(repository.Status.VolumeClaimName), updated: updated,
		})
	}
	slices.SortFunc(rows, func(left repositoryListRow, right repositoryListRow) int {
		return strings.Compare(left.name, right.name)
	})

	return rows
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}
