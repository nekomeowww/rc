package repositories

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/command"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	clioutput "github.com/nekomeowww/rc/pkg/output"
)

type listOptions struct {
	output clioutput.Options
}

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
	options := new(listOptions)
	cmd := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List Repositories in the current namespace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := options.output.Validate(true); err != nil {
				return err
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

			return runRepositoryList(cmd.Context(), cmd.OutOrStdout(), kubeClient, namespace, options.output)
		},
	}
	options.output.AddFlags(cmd, true)

	return cmd
}

func newGetCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := new(clioutput.Options)
	cmd := &cobra.Command{
		Use: "get NAME", Short: "Show a Repository", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := options.Validate(false); err != nil {
				return err
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
			repository := new(repositoriesv1alpha1.Repository)
			if err := kubeClient.Get(cmd.Context(), client.ObjectKey{Namespace: namespace, Name: args[0]}, repository); err != nil {
				return fmt.Errorf("get Repository %q: %w", args[0], err)
			}
			return options.PrintDetails(cmd.OutOrStdout(), repository, kubeClient.Scheme(), repositoryDetailFields(repository))
		},
	}
	options.AddFlags(cmd, false)

	return cmd
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

func runRepositoryList(ctx context.Context, writer io.Writer, kubeClient client.Client, namespace string, options clioutput.Options) error {
	list := new(repositoriesv1alpha1.RepositoryList)
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list Repositories: %w", err)
	}
	list.Items = repositoryListItems(list.Items)
	rows := make([][]any, 0, len(list.Items))
	for _, row := range repositoryListRows(list.Items) {
		rows = append(rows, []any{row.name, row.ready, row.remote, row.ref, row.volume, row.updated})
	}

	return options.PrintList(writer, list, kubeClient.Scheme(), clioutput.Table{
		Columns: []clioutput.Column{
			{Name: "NAME", MaxWidth: 32}, {Name: "READY"}, {Name: "REMOTE", MinWidth: 12, MaxWidth: 48, Flexible: true},
			{Name: "REF", MaxWidth: 32}, {Name: "VOLUME", MaxWidth: 32, Wide: true}, {Name: "UPDATED"},
		},
		Rows: rows,
	})
}

func repositoryListItems(repositories []repositoriesv1alpha1.Repository) []repositoriesv1alpha1.Repository {
	items := slices.Clone(repositories)
	slices.SortFunc(items, func(left repositoriesv1alpha1.Repository, right repositoriesv1alpha1.Repository) int {
		return strings.Compare(left.Name, right.Name)
	})

	return items
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

func repositoryDetailFields(repository *repositoriesv1alpha1.Repository) []clioutput.Field {
	credential := "-"
	if repository.Spec.Remote.CredentialRef != nil {
		credential = clioutput.ValueOrDash(repository.Spec.Remote.CredentialRef.Name)
	}
	submodules := "disabled"
	if repository.Spec.Submodules != nil {
		submodules = "direct"
		if repository.Spec.Submodules.Recursive {
			submodules = "recursive"
		}
	}

	return []clioutput.Field{
		{Name: "Name", Value: repository.Name},
		{Name: "Namespace", Value: repository.Namespace},
		{Name: "Created", Value: clioutput.Timestamp(repository.CreationTimestamp)},
		{Name: "Ready", Value: meta.IsStatusConditionTrue(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)},
		{Name: "Remote", Value: clioutput.ValueOrDash(repository.Spec.Remote.URL)},
		{Name: "Ref", Value: clioutput.ValueOrDash(repository.Spec.Ref)},
		{Name: "Credential", Value: credential},
		{Name: "Allow insecure HTTP", Value: repository.Spec.Remote.AllowInsecureHTTP},
		{Name: "Submodules", Value: submodules},
		{Name: "Storage class", Value: clioutput.ValueOrDash(repository.Spec.Storage.StorageClassName)},
		{Name: "Size", Value: repository.Spec.Storage.Size.String()},
		{Name: "Volume", Value: clioutput.ValueOrDash(repository.Status.VolumeClaimName)},
		{Name: "Last updated", Value: clioutput.OptionalTimestamp(repository.Status.LastUpdatedAt)},
		{Name: "Conditions", Value: clioutput.Conditions(repository.Status.Conditions)},
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}
