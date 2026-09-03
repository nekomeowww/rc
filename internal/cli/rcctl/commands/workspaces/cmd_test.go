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

package workspaces

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

const (
	testWorkspaceNamespace      = "development"
	testWorkspaceName           = "dev"
	testPersonalAgentCredential = "codex-personal"
	testTeamAgentCredential     = "codex-team"
	testWorkspaceCredential     = "github-ssh"
)

func TestSetWorkspaceCredentialReferences(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&configsv1alpha1.AgentCredential{ObjectMeta: metav1.ObjectMeta{Name: testPersonalAgentCredential, Namespace: testWorkspaceNamespace}},
		&configsv1alpha1.AgentCredential{ObjectMeta: metav1.ObjectMeta{Name: testTeamAgentCredential, Namespace: testWorkspaceNamespace}},
		&configsv1alpha1.Credential{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceCredential, Namespace: testWorkspaceNamespace}},
	).Build()
	workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}

	err := setWorkspaceCredentialReferences(
		context.Background(), kubeClient, workspace,
		[]string{testPersonalAgentCredential, testTeamAgentCredential}, []string{testWorkspaceCredential},
	)
	require.NoError(t, err, "attach existing credentials to a Workspace")

	assert.Equal(t, []workspacesv1alpha1.LocalReference{{Name: testPersonalAgentCredential}, {Name: testTeamAgentCredential}}, workspace.Spec.AgentCredentialRefs, "preserve AgentCredential order")
	assert.Equal(t, []workspacesv1alpha1.LocalReference{{Name: testWorkspaceCredential}}, workspace.Spec.CredentialRefs, "attach generic Credentials")
}

func TestSetWorkspaceCredentialReferencesRejectsMissingResource(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, configsv1alpha1.AddToScheme(scheme), "register config API types")
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	t.Run("AgentCredential", func(t *testing.T) {
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}
		err := setWorkspaceCredentialReferences(context.Background(), kubeClient, workspace, []string{"missing"}, nil)

		require.Error(t, err, "reject a missing AgentCredential")
		assert.ErrorContains(t, err, `get AgentCredential "missing"`, "identify the missing AgentCredential")
		assert.Empty(t, workspace.Spec.AgentCredentialRefs, "do not partially attach AgentCredentials")
	})

	t.Run("Credential", func(t *testing.T) {
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testWorkspaceNamespace}}
		err := setWorkspaceCredentialReferences(context.Background(), kubeClient, workspace, nil, []string{"missing"})

		require.Error(t, err, "reject a missing Credential")
		assert.ErrorContains(t, err, `get Credential "missing"`, "identify the missing Credential")
		assert.Empty(t, workspace.Spec.CredentialRefs, "do not partially attach Credentials")
	})
}

func TestWorkspaceListItemsSortsByName(t *testing.T) {
	t.Parallel()
	items := workspaceListItems([]workspacesv1alpha1.Workspace{
		{ObjectMeta: metav1.ObjectMeta{Name: "zeta"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
	})

	require.Len(t, items, 2, "retain every Workspace")
	assert.Equal(t, "alpha", items[0].Name, "sort inventory by name")
	assert.Equal(t, "zeta", items[1].Name, "retain the second sorted Workspace")
}

func TestWorkspaceListTableUsesHumanAgeAndWideMetadata(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	table := workspaceListTable([]workspacesv1alpha1.Workspace{{
		ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, CreationTimestamp: metav1.NewTime(now.Add(-2 * time.Hour))},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning, Generated: true,
		},
	}}, now)

	require.Len(t, table.Rows, 1, "create one row per Workspace")
	assert.Equal(t, "120m", table.Rows[0][7], "use a compact Kubernetes-style age")
	assert.True(t, table.Columns[3].Wide, "keep generated metadata in wide output")
}

func TestWorkspaceListAndGetCommandsExposeOutputFormats(t *testing.T) {
	t.Parallel()
	listCommand := newListCommand(kubeconfig.NewFlags())
	getCommand := newGetCommand(kubeconfig.NewFlags())

	assert.Contains(t, listCommand.Aliases, "ls", "offer the conventional list alias")
	require.NotNil(t, listCommand.Flag("output"), "list accepts an output format")
	require.NotNil(t, getCommand.Flag("output"), "get accepts a structured output format")
	require.NoError(t, getCommand.Args(getCommand, []string{testWorkspaceName}), "get accepts exactly one Workspace name")
	require.Error(t, getCommand.Args(getCommand, nil), "get requires a Workspace name")
}
