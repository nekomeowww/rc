package kubeconfig

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Flags holds kubectl-compatible connection overrides shared by rcctl commands.
type Flags struct {
	kubeconfig string
	context    string
	namespace  string
}

// NewFlags creates an unset group of Kubernetes client flags.
func NewFlags() *Flags {
	return &Flags{}
}

// AddFlags attaches Kubernetes connection flags to a command flag set.
func (flags *Flags) AddFlags(flagSet *pflag.FlagSet) {
	flagSet.StringVar(&flags.kubeconfig, "kubeconfig", "", "Path to the kubeconfig file")
	flagSet.StringVar(&flags.context, "context", "", "Name of the kubeconfig context to use")
	flagSet.StringVarP(&flags.namespace, "namespace", "n", "", "Namespace for the request")
}

// Resolve loads a REST config and namespace using explicit flags, in-cluster
// configuration, KUBECONFIG, and the default kubeconfig in that order.
func (flags *Flags) Resolve() (*rest.Config, string, error) {
	// NOTICE: This precedence is adapted from BaizeAI/dataset, but rcctl keeps
	// explicit-path errors instead of silently falling back to another cluster.
	// https://github.com/BaizeAI/dataset/blob/298e3ce6a397fd63f48052d656a70a52ba17befa/pkg/clients/kubeconfig.go#L10-L41
	if flags.kubeconfig == "" && flags.context == "" {
		if config, err := rest.InClusterConfig(); err == nil {
			return rest.AddUserAgent(config, "rcctl"), flags.inClusterNamespace(), nil
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = flags.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: flags.context}
	overrides.Context.Namespace = flags.namespace
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	config, err := loader.ClientConfig()
	if err != nil {
		if clientcmd.IsEmptyConfig(err) {
			return nil, "", errors.New("no Kubernetes configuration found; set --kubeconfig or KUBECONFIG")
		}
		return nil, "", fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	namespace, _, err := loader.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("resolve Kubernetes namespace: %w", err)
	}

	return rest.AddUserAgent(config, "rcctl"), namespace, nil
}

func (flags *Flags) inClusterNamespace() string {
	if flags.namespace != "" {
		return flags.namespace
	}

	data, err := os.ReadFile(serviceAccountNamespacePath)
	if err == nil {
		if namespace := strings.TrimSpace(string(data)); namespace != "" {
			return namespace
		}
	}

	return metav1.NamespaceDefault
}
