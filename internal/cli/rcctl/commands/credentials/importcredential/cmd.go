package importcredential

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	credentialservice "github.com/nekomeowww/rc/internal/credentials"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

type options struct {
	credentialType string
	agent          string
	file           string
}

// NewCommand creates the credential import command.
func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	commandOptions := new(options)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import credential data into rc resources",
		Args:  cobra.NoArgs,
		PreRunE: func(*cobra.Command, []string) error {
			return commandOptions.validate()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, kubeconfigFlags, *commandOptions)
		},
	}
	cmd.Flags().StringVar(&commandOptions.credentialType, "type", "", "Credential type to import")
	cmd.Flags().StringVar(&commandOptions.agent, "agent", "", "Agent credential format")
	cmd.Flags().StringVar(&commandOptions.file, "file", "", "Path to the credential file")
	return cmd
}

func (options options) validate() error {
	// TODO(generic-credential-import): Credential API imports are deferred until
	// the owner specifies their type-specific file semantics and naming policy.
	if options.credentialType != "agent" {
		return fmt.Errorf("unsupported credential type %q: only agent is supported", options.credentialType)
	}
	if options.agent != string(configsv1alpha1.AgentTypeCodex) {
		return fmt.Errorf("unsupported agent %q: only codex is supported", options.agent)
	}
	if options.file == "" {
		return errors.New("flag --file is required")
	}
	return nil
}

func run(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, options options) error {
	data, err := os.ReadFile(options.file)
	if err != nil {
		return fmt.Errorf("read credential file: %w", err)
	}

	config, namespace, err := kubeconfigFlags.Resolve()
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register Kubernetes API types: %w", err)
	}
	if err := configsv1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("register rc API types: %w", err)
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	result, err := credentialservice.NewImporter(kubeClient, scheme).ImportAgent(cmd.Context(), credentialservice.ImportAgentRequest{
		Namespace: namespace,
		Agent:     configsv1alpha1.AgentType(options.agent),
		Data:      data,
	})
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "agentcredential.configs.rc.ayaka.io/%s %s\n", result.AgentCredentialName, result.AgentCredentialOperation); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "secret/%s %s\n", result.SecretName, result.SecretOperation)
	return err
}
