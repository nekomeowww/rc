/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agents

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/kubeconfig"
)

const testAgentCommand = "codex"
const testTaskArgument = "task"
const testWorkspaceFlag = "--workspace"
const testDevelopmentName = "dev"

func TestRunCommandStopsParsingRcctlFlagsAtCommand(t *testing.T) {
	command := newRunCommand(kubeconfig.NewFlags(), true)
	require.NoError(t, command.ParseFlags([]string{testWorkspaceFlag, testDevelopmentName, testAgentCommand, "--dangerously-bypass-approvals-and-sandbox", testTaskArgument}))
	require.Equal(t, testDevelopmentName, command.Flag("workspace").Value.String())
	require.Equal(t, []string{testAgentCommand, "--dangerously-bypass-approvals-and-sandbox", testTaskArgument}, command.Flags().Args())
}

func TestRunCommandAcceptsOptionalSeparator(t *testing.T) {
	command := newRunCommand(kubeconfig.NewFlags(), true)
	require.NoError(t, command.ParseFlags([]string{testWorkspaceFlag, testDevelopmentName, "--", testAgentCommand, testTaskArgument}))
	require.Equal(t, []string{testAgentCommand, testTaskArgument}, command.Flags().Args())
}

func TestRunCommandAcceptsDetachFlag(t *testing.T) {
	command := newRunCommand(kubeconfig.NewFlags(), true)
	require.NoError(t, command.ParseFlags([]string{"-d", testWorkspaceFlag, testDevelopmentName, "--", testAgentCommand, testTaskArgument}))

	detach := command.Flag("detach")
	require.NotNil(t, detach)
	assert.Equal(t, "d", detach.Shorthand)
	assert.Equal(t, "true", detach.Value.String())
}

func TestExecCommandDoesNotAcceptDetachFlag(t *testing.T) {
	command := newRunCommand(kubeconfig.NewFlags(), false)
	err := command.ParseFlags([]string{"--detach", "--", testAgentCommand, testTaskArgument})
	require.Error(t, err)
}

func TestAgentListItemsFiltersAndSortsOldestFirst(t *testing.T) {
	t.Parallel()
	oldest := metav1.NewTime(time.Date(2026, time.September, 1, 1, 0, 0, 0, time.UTC))
	newest := metav1.NewTime(oldest.Add(2 * time.Minute))
	processes := []workspacesv1alpha1.AgentProcess{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "codex-new", Namespace: testDevelopmentName, CreationTimestamp: newest},
			Spec: workspacesv1alpha1.AgentProcessSpec{
				TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Name: "other"}, AgentType: testAgentCommand,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "process-old", Namespace: testDevelopmentName, CreationTimestamp: oldest},
			Spec: workspacesv1alpha1.AgentProcessSpec{
				TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Name: testDevelopmentName}, AgentType: testAgentCommand,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "process-filtered", Namespace: testDevelopmentName, CreationTimestamp: oldest},
			Spec: workspacesv1alpha1.AgentProcessSpec{
				TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Name: testDevelopmentName}, AgentType: "shell",
			},
		},
	}

	items := agentListItems(processes, listOptions{agent: testAgentCommand})

	require.Len(t, items, 2, "retain only the selected agent type")
	assert.Equal(t, "process-old", items[0].Name, "put the oldest process first despite its name prefix")
	assert.Equal(t, "codex-new", items[1].Name, "put the newest process last")
}

func TestAgentListAndGetCommandsExposeOutputFormats(t *testing.T) {
	t.Parallel()
	listCommand := newListCommand(kubeconfig.NewFlags())
	getCommand := newGetCommand(kubeconfig.NewFlags())

	assert.Contains(t, listCommand.Aliases, "ls", "offer the conventional list alias")
	require.NotNil(t, listCommand.Flag("output"), "list accepts an output format")
	require.NotNil(t, getCommand.Flag("output"), "get accepts a structured output format")
	require.NoError(t, getCommand.Args(getCommand, []string{"process-id"}), "get accepts exactly one process ID")
	require.Error(t, getCommand.Args(getCommand, nil), "get requires a process ID")
}

func TestSelectAgentCredentialsKeepsMixedWorkspaceSetAndSelectsCompatibleCredential(t *testing.T) {
	t.Parallel()
	const customAgent = "custom-agent"
	const customCredential = "custom-credential"
	scheme := runtime.NewScheme()
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&configsv1alpha1.AgentCredential{
			ObjectMeta: metav1.ObjectMeta{Name: customCredential, Namespace: testDevelopmentName},
			Spec:       configsv1alpha1.AgentCredentialSpec{Agent: configsv1alpha1.AgentType(customAgent)},
		},
		&configsv1alpha1.AgentCredential{
			ObjectMeta: metav1.ObjectMeta{Name: testAgentCommand, Namespace: testDevelopmentName},
			Spec:       configsv1alpha1.AgentCredentialSpec{Agent: configsv1alpha1.AgentTypeCodex},
		},
	).Build()

	names, selected, selectedType, err := selectAgentCredentials(
		context.Background(), kubeClient, testDevelopmentName, customAgent, []string{testAgentCommand, customCredential}, true,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{testAgentCommand, customCredential}, names, "retain every explicitly granted Workspace credential")
	assert.Equal(t, customCredential, selected, "select the first credential compatible with the process command")
	assert.Equal(t, customAgent, selectedType, "derive the selected process credential type")
}

func TestSelectAgentCredentialsUsesExplicitCredentialForOrdinaryCommand(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&configsv1alpha1.AgentCredential{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentCommand, Namespace: testDevelopmentName},
		Spec:       configsv1alpha1.AgentCredentialSpec{Agent: configsv1alpha1.AgentTypeCodex},
	}).Build()

	names, selected, selectedType, err := selectAgentCredentials(
		context.Background(), kubeClient, testDevelopmentName, "", []string{testAgentCommand}, true,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{testAgentCommand}, names, "grant the explicitly selected credential to the Workspace")
	assert.Equal(t, testAgentCommand, selected, "attach the explicit credential to an ordinary process")
	assert.Equal(t, string(configsv1alpha1.AgentTypeCodex), selectedType, "derive process configuration from the credential")
}
