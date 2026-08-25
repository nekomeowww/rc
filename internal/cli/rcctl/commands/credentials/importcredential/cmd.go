package importcredential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	credentialservice "github.com/nekomeowww/rc/internal/credentials"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

type options struct {
	credentialType string
	agent          string
	file           string
	hostname       string
	name           string
	mountPath      string
	environment    []string
}

const (
	credentialTypeAgent   = "agent"
	credentialTypeProcess = "process"
	credentialTypeGitHub  = "github"
	defaultGitHubHost     = "github.com"
)

// NewCommand creates the credential import command.
func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	commandOptions := &options{hostname: defaultGitHubHost}
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
	cmd.Flags().StringVar(&commandOptions.hostname, "hostname", defaultGitHubHost, "GitHub hostname for --type github")
	cmd.Flags().StringVar(&commandOptions.name, "name", "", "Credential metadata.name; derive it from the GitHub hostname when omitted")
	cmd.Flags().StringVar(&commandOptions.mountPath, "mount-path", "", "Absolute process-scoped file path for --type process")
	cmd.Flags().StringArrayVar(&commandOptions.environment, "env", nil, "Literal NAME=value added when the process Credential is selected; repeat")
	return cmd
}

func (options options) validate() error {
	switch options.credentialType {
	case credentialTypeAgent:
		agent := configsv1alpha1.AgentType(options.agent)
		if agent != configsv1alpha1.AgentTypeCodex {
			return fmt.Errorf("unsupported agent %q: supported agent is codex", options.agent)
		}
		if options.file == "" {
			return errors.New("flag --file is required")
		}
		return nil
	case credentialTypeProcess:
		if options.name == "" {
			return errors.New("flag --name is required")
		}
		if validationErrors := validation.IsDNS1123Subdomain(options.name); len(validationErrors) > 0 {
			return fmt.Errorf("invalid Credential name %q: %s", options.name, validationErrors[0])
		}
		if options.file == "" {
			return errors.New("flag --file is required")
		}
		if options.mountPath == "" {
			return errors.New("flag --mount-path is required")
		}
		if !filepath.IsAbs(options.mountPath) || filepath.Clean(options.mountPath) != options.mountPath || options.mountPath == string(filepath.Separator) {
			return fmt.Errorf("credential mount path %q must be a clean absolute file path", options.mountPath)
		}
		_, err := parseCredentialEnvironment(options.environment)
		return err
	case credentialTypeGitHub:
		if options.file != "" {
			return errors.New("flag --file is only supported with --type agent or process")
		}
		if hostname := strings.TrimSpace(options.hostname); hostname == "" || strings.ContainsAny(hostname, "/?#") {
			return fmt.Errorf("invalid GitHub hostname %q", options.hostname)
		}
		if options.name != "" {
			if validationErrors := validation.IsDNS1123Subdomain(options.name); len(validationErrors) > 0 {
				return fmt.Errorf("invalid Credential name %q: %s", options.name, validationErrors[0])
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported credential type %q: only agent, process, and github are supported", options.credentialType)
	}
}

func run(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, options options) error {
	var (
		data  []byte
		token string
		err   error
	)
	switch options.credentialType {
	case credentialTypeAgent, credentialTypeProcess:
		data, err = os.ReadFile(options.file)
		if err != nil {
			return fmt.Errorf("read credential file: %w", err)
		}
	case credentialTypeGitHub:
		token, err = readGitHubToken(cmd.Context(), options.hostname)
		if err != nil {
			return err
		}
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

	importer := credentialservice.NewImporter(kubeClient, scheme)
	if options.credentialType == credentialTypeGitHub {
		result, err := importer.ImportGitHub(cmd.Context(), credentialservice.ImportGitHubRequest{
			Namespace: namespace,
			Hostname:  options.hostname,
			Name:      options.name,
			Token:     token,
		})
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "credential.configs.rc.ayaka.io/%s %s\n", result.CredentialName, result.CredentialOperation); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "secret/%s %s\n", result.SecretName, result.SecretOperation)
		return err
	}
	if options.credentialType == credentialTypeProcess {
		environment, err := parseCredentialEnvironment(options.environment)
		if err != nil {
			return err
		}
		result, err := importer.ImportProcess(cmd.Context(), credentialservice.ImportProcessRequest{
			Namespace: namespace, Name: options.name, Data: data, MountPath: options.mountPath, Envs: environment,
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "credential.configs.rc.ayaka.io/%s %s\n", result.CredentialName, result.CredentialOperation); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "secret/%s %s\n", result.SecretName, result.SecretOperation)
		return err
	}

	result, err := importer.ImportAgent(cmd.Context(), credentialservice.ImportAgentRequest{
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

func parseCredentialEnvironment(values []string) ([]configsv1alpha1.CredentialEnv, error) {
	environment := make([]configsv1alpha1.CredentialEnv, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		name, literal, found := strings.Cut(value, "=")
		if !found {
			return nil, fmt.Errorf("credential environment %q must use NAME=value", value)
		}
		if validationErrors := validation.IsEnvVarName(name); len(validationErrors) > 0 {
			return nil, fmt.Errorf("invalid credential environment variable %q: %s", name, validationErrors[0])
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("credential environment variable %q is repeated", name)
		}
		seen[name] = struct{}{}
		environment = append(environment, configsv1alpha1.CredentialEnv{Name: name, Value: literal})
	}
	return environment, nil
}

func readGitHubToken(ctx context.Context, hostname string) (string, error) {
	output, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", hostname).Output()
	if err != nil {
		return "", fmt.Errorf("read GitHub token with gh: %w", err)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", errors.New("gh returned an empty GitHub token")
	}

	return token, nil
}
