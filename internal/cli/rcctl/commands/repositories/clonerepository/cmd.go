package clonerepository

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	repositoryservice "github.com/nekomeowww/rc/internal/repositories"
)

type cloneOptions struct {
	name          string
	storageClass  string
	size          string
	ref           string
	credentialRef string
	wait          bool
}

// NewCommand creates the Repository clone command.
func NewCommand(kubeconfigFlags *kubeconfig.Flags) *cobra.Command {
	options := &cloneOptions{wait: true}
	command := &cobra.Command{
		Use:   "clone URL",
		Short: "Create a Repository from a Git remote",
		Args:  cobra.ExactArgs(1),
		PreRunE: func(*cobra.Command, []string) error {
			return options.validate()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, kubeconfigFlags, *options, args[0])
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.name, "name", "", "Repository metadata.name; derive it from the Git host and path when omitted")
	flags.StringVar(&options.storageClass, "storage-class", "", "StorageClass for the Repository parent PVC")
	flags.StringVar(&options.size, "size", "", "Requested size of the Repository parent PVC")
	flags.StringVar(&options.ref, "ref", "", "Full Git ref or commit to synchronize")
	flags.StringVar(&options.credentialRef, "credential-ref", "", "Credential resource name for the Git remote")
	flags.BoolVar(&options.wait, "wait", true, "Wait for Repository bootstrap to complete")
	_ = command.MarkFlagRequired("storage-class")
	_ = command.MarkFlagRequired("size")

	return command
}

func (options cloneOptions) validate() error {
	if options.name != "" {
		if errors := validation.IsDNS1123Subdomain(options.name); len(errors) > 0 {
			return fmt.Errorf("invalid Repository name %q: %s", options.name, errors[0])
		}
	}
	_, err := parseSize(options.size)
	return err
}

func run(cmd *cobra.Command, kubeconfigFlags *kubeconfig.Flags, options cloneOptions, remoteURL string) error {
	size, err := parseSize(options.size)
	if err != nil {
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

	repositoryClient := &repositoryservice.RepositoryClient{Client: kubeClient}
	repository, err := repositoryClient.Clone(cmd.Context(), repositoryservice.CloneRequest{
		Namespace:     namespace,
		URL:           remoteURL,
		Name:          options.name,
		Ref:           options.ref,
		StorageClass:  options.storageClass,
		Size:          size,
		CredentialRef: options.credentialRef,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "repository.repositories.rc.ayaka.io/%s created\n", repository.Name); err != nil {
		return err
	}
	if !options.wait {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), repository.Name)
		return err
	}

	if err := repositoryClient.Wait(cmd.Context(), repository, cmd.OutOrStdout()); err != nil {
		return err
	}

	current := new(repositoriesv1alpha1.Repository)
	if err := kubeClient.Get(cmd.Context(), client.ObjectKeyFromObject(repository), current); err != nil {
		return fmt.Errorf("get ready Repository: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "repository/%s ready pvc/%s\n", current.Name, current.Status.VolumeClaimName)

	return err
}

func parseSize(value string) (resource.Quantity, error) {
	if value == "" {
		return resource.Quantity{}, fmt.Errorf("--size is required")
	}

	parsed, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parse --size: %w", err)
	}
	if parsed.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("--size must be greater than zero")
	}

	return parsed, nil
}
