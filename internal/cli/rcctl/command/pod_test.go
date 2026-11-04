package command

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func parseRuntimePodFlags(t *testing.T, arguments ...string) RuntimePodOptions {
	t.Helper()
	flags := pflag.NewFlagSet("runtime-pod", pflag.ContinueOnError)
	options := new(RuntimePodOptions)
	options.AddFlags(flags)
	require.NoError(t, flags.Parse(arguments))
	return *options
}

func TestRuntimePodOptionsBuildMetadataAndPriority(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	options := parseRuntimePodFlags(t,
		"--pod-label=example.com/tenant=team-a",
		"--pod-label=billing=enabled",
		"--pod-annotation", "karpenter.sh/do-not-disrupt=true",
		"--pod-annotation", "example.com/note=a,b=c",
		"--priority-class", "workspace-high",
	)

	metadata, priorityClass, err := options.Resolve()

	require.NoError(t, err)
	assertions.Equal("workspace-high", priorityClass)
	assertions.Equal(&workspacesv1alpha1.WorkspacePodMetadata{
		Labels: map[string]string{
			"example.com/tenant": "team-a",
			"billing":            "enabled",
		},
		Annotations: map[string]string{
			"karpenter.sh/do-not-disrupt": "true",
			"example.com/note":            "a,b=c",
		},
	}, metadata, "repeat each flag and keep commas inside annotation values")
}

func TestRuntimePodOptionsOmitEmptyMetadata(t *testing.T) {
	t.Parallel()
	metadata, priorityClass, err := parseRuntimePodFlags(t).Resolve()

	require.NoError(t, err)
	assert.Nil(t, metadata, "omit podMetadata without flags")
	assert.Empty(t, priorityClass)
}

func TestRuntimePodOptionsRejectInvalidValues(t *testing.T) {
	t.Parallel()
	for name, arguments := range map[string][]string{
		"missing value separator": {"--pod-label=tenant"},
		"reserved label":          {"--pod-label=workspaces.rc.ayaka.io/workspace=other"},
		"reserved annotation":     {"--pod-annotation", "rc.ayaka.io/repository=other"},
		"duplicate key":           {"--pod-label=tenant=a", "--pod-label=tenant=b"},
		"invalid label value":     {"--pod-label=tenant=team a"},
		"invalid annotation key":  {"--pod-annotation", "not a key=value"},
		"invalid priority class":  {"--priority-class", "Workspace_High"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseRuntimePodFlags(t, arguments...).Resolve()
			assert.Error(t, err)
		})
	}
}
