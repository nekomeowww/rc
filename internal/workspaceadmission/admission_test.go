package workspaceadmission

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

func admissionObjects(t *testing.T) (*runtime.Scheme, *workspaces.Workspace, *workspaces.WorkspaceExec) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, workspaces.AddToScheme(scheme))
	workspace := &workspaces.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "development", UID: "workspace-uid"}}
	process := &workspaces.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "command", Namespace: workspace.Namespace, UID: "execution-uid", Finalizers: []string{"workspaces.rc.ayaka.io/workspace-exec"}}, Spec: workspaces.WorkspaceExecSpec{TargetRef: workspaces.WorkspaceExecTargetReference{Kind: workspaces.WorkspaceExecTargetWorkspace, Name: workspace.Name}, Command: []string{"true"}}}
	return scheme, workspace, process
}

// ROOT CAUSE: reading an open owner is not admission. Closure must win against
// the conditional write even when it occurs after that read.
func TestAdmissionConflictsWithClosureAfterRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme, workspace, process := admissionObjects(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace, process).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, kubeClient client.Client, subResource string, object client.Object, opts ...client.SubResourceUpdateOption) error {
			current := new(workspaces.Workspace)
			require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), current))
			require.NoError(t, (Gate{Client: kubeClient}).Close(ctx, current))
			return kubeClient.SubResource(subResource).Update(ctx, object, opts...)
		},
	}).Build()
	err := (Gate{Client: kubeClient}).Admit(ctx, workspace, process)
	require.True(t, apierrors.IsConflict(err), "%v", err)
	current := new(workspaces.Workspace)
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), current))
	assert.True(t, current.Status.ExecutionAdmissionClosed)
}

func TestReopeningCancelsPreparedDeleteAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme, workspace, process := admissionObjects(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace, process).Build()
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	gate := Gate{Client: kubeClient}
	require.NoError(t, gate.Close(ctx, workspace))
	preparedDelete := client.Preconditions{UID: &workspace.UID, ResourceVersion: &workspace.ResourceVersion}
	restarted := Gate{Client: kubeClient}
	require.ErrorIs(t, restarted.Admit(ctx, workspace, process), ErrClosed)
	require.NoError(t, restarted.Reopen(ctx, workspace))
	require.NoError(t, restarted.Admit(ctx, workspace, process))
	require.True(t, apierrors.IsConflict(kubeClient.Delete(ctx, workspace, preparedDelete)))
}

func TestAdmissionCannotRetargetRecreatedOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme, workspace, process := admissionObjects(t)
	replacement := workspace.DeepCopy()
	replacement.UID = "replacement-uid"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(replacement, process).Build()
	gate := Gate{Client: kubeClient}
	require.ErrorIs(t, gate.Admit(ctx, workspace, process), ErrOwnerChanged)
	controller := true
	process.OwnerReferences = []metav1.OwnerReference{{APIVersion: workspaces.SchemeGroupVersion.String(), Kind: "Workspace", Name: workspace.Name, UID: workspace.UID, Controller: &controller}}
	require.ErrorIs(t, gate.Admit(ctx, replacement, process), ErrOwnerChanged)
}

func TestCloseFailsWhenServerDoesNotPersistFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme, workspace, _ := admissionObjects(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(workspace).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, kubeClient client.Client, subResource string, object client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
			// Model an old CRD pruning the new status field. Do not clear the caller's
			// object: decoding an omitted field into it can retain the requested value.
			persisted := object.DeepCopyObject().(*workspaces.Workspace)
			persisted.Status.ExecutionAdmissionClosed = false
			return kubeClient.SubResource(subResource).Update(ctx, persisted)
		},
	}).Build()
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	require.Error(t, (Gate{Client: kubeClient}).Close(ctx, workspace), "pruned fences must never authorize automatic deletion")
}
