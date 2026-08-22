package kubeconfig

import (
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const kubeconfigFlagName = "--kubeconfig"

func TestResolveUsesExplicitKubeconfigOverrides(t *testing.T) {
	t.Parallel()
	const contextName = "first"

	kubeconfigPath := filepath.Join(t.TempDir(), "config")
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"test-cluster": {Server: "https://127.0.0.1:6443"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"test-user": {Token: "test-token"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			contextName: {
				Cluster:   "test-cluster",
				AuthInfo:  "test-user",
				Namespace: "from-context",
			},
		},
		CurrentContext: contextName,
	}, kubeconfigPath), "write kubeconfig fixture")

	flags := NewFlags()
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.AddFlags(flagSet)
	require.NoError(t, flagSet.Parse([]string{
		kubeconfigFlagName, kubeconfigPath,
		"--context", contextName,
		"--namespace", "from-flag",
	}), "parse kubeconfig flags")

	config, namespace, err := flags.Resolve()
	require.NoError(t, err, "resolve explicit kubeconfig")
	assert.Equal(t, "https://127.0.0.1:6443", config.Host, "use selected cluster")
	assert.Equal(t, "from-flag", namespace, "explicit namespace overrides context")
}

func TestResolveWithIdentityReturnsSelectedContext(t *testing.T) {
	t.Parallel()
	const contextName = "workspace-context"
	kubeconfigPath := filepath.Join(t.TempDir(), "config")
	require.NoError(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters:  map[string]*clientcmdapi.Cluster{"cluster": {Server: "https://127.0.0.1:6443"}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"user": {Token: "token"}},
		Contexts: map[string]*clientcmdapi.Context{contextName: {
			Cluster: "cluster", AuthInfo: "user", Namespace: "development",
		}},
		CurrentContext: contextName,
	}, kubeconfigPath), "write kubeconfig fixture")
	flags := NewFlags()
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.AddFlags(flagSet)
	require.NoError(t, flagSet.Parse([]string{kubeconfigFlagName, kubeconfigPath}), "parse kubeconfig flags")

	_, namespace, selectedContext, err := flags.ResolveWithIdentity()
	require.NoError(t, err, "resolve kubeconfig identity")
	assert.Equal(t, "development", namespace)
	assert.Equal(t, contextName, selectedContext)
}

func TestResolveRejectsMissingExplicitKubeconfig(t *testing.T) {
	t.Parallel()

	flags := NewFlags()
	flagSet := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.AddFlags(flagSet)
	require.NoError(t, flagSet.Parse([]string{
		kubeconfigFlagName, filepath.Join(t.TempDir(), "missing"),
	}), "parse kubeconfig flags")

	config, namespace, err := flags.Resolve()
	require.Error(t, err, "an explicit missing path must not fall back")
	assert.Nil(t, config)
	assert.Empty(t, namespace)
}
