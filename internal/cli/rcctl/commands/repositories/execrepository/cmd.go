package execrepository

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
)

type options struct {
	wait bool
}

// NewCommand creates the repository exec command.
func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	commandOptions := &options{wait: true}
	cmd := &cobra.Command{
		Use:   "exec REPOSITORY -- COMMAND [ARG...]",
		Short: "Execute an exact command in a repository",
		Args:  exactCommandArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, kubeconfigFlags, *commandOptions, args[0], args[1:])
		},
	}

	cmd.Flags().BoolVar(&commandOptions.wait, "wait", true, "Wait for completion and write command output")
	return cmd
}

func exactCommandArgs(cmd *cobra.Command, args []string) error {
	if cmd.ArgsLenAtDash() != 1 {
		return errors.New("expected exactly one Repository name before --")
	}
	if len(args) < 2 {
		return errors.New("expected a command after --")
	}

	return nil
}

func run(
	cmd *cobra.Command,
	kubeconfigFlags *kubeconfig.Flags,
	options options,
	repositoryName string,
	command []string,
) error {
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

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes clientset: %w", err)
	}

	execClient := &repositoryservice.ExecClient{Client: kubeClient, Kubernetes: clientset}
	exec, err := execClient.Start(cmd.Context(), repositoryservice.ExecRequest{
		Namespace:  namespace,
		Repository: repositoryName,
		Command:    command,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "repositoryexec.repositories.rc.ayaka.io/%s created\n", exec.Name); err != nil {
		return err
	}
	if !options.wait {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), exec.Name)
		return err
	}

	return execClient.Wait(cmd.Context(), exec, cmd.OutOrStdout())
}
