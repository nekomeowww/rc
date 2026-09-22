package workspaces

import (
	"testing"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestWorkspaceReleasesParentBeforeDependencyChecks(t *testing.T) {
	const missingEnvironmentReason = "EnvironmentNotFound"
	t.Parallel()
	for _, wantReason := range []string{missingEnvironmentReason, "WorktreeNotFound"} {
		t.Run(wantReason, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, repositoriesv1alpha1.AddToScheme, workspacesv1alpha1.AddToScheme} {
				require.NoError(t, add(scheme))
			}
			parent := &repositoriesv1alpha1.Repository{
				ObjectMeta: metav1.ObjectMeta{Name: "mounted-parent", Namespace: testNamespace, UID: "parent-uid", Generation: 1},
				Status: repositoriesv1alpha1.RepositoryStatus{ObservedGeneration: 1, VolumeClaimName: "mounted-parent", Conditions: []metav1.Condition{{
					Type: repositoriesv1alpha1.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1,
				}}},
			}
			workspace := &workspacesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "dependency-blocked", Namespace: testNamespace, UID: "workspace"},
				Spec: workspacesv1alpha1.WorkspaceSpec{
					Image:        testRuntimeImage,
					DesiredState: workspacesv1alpha1.WorkspaceDesiredStateSuspended,
					Storage:      &workspacesv1alpha1.PersistentStorageSpec{StorageClassName: testStorageClass, Size: resource.MustParse("1Gi")},
					Mounts:       []workspacesv1alpha1.WorkspaceMount{{Name: "parent-mount", Path: "parent", RepositoryRef: &workspacesv1alpha1.LocalReference{Name: parent.Name}}},
				},
			}
			if wantReason == missingEnvironmentReason {
				workspace.Spec.EnvironmentRef = &workspacesv1alpha1.LocalReference{Name: "missing-environment"}
			} else {
				workspace.Spec.Mounts = append(workspace.Spec.Mounts, workspacesv1alpha1.WorkspaceMount{Name: "child-mount", Path: "child", WorktreeRef: &workspacesv1alpha1.LocalReference{Name: "missing-child"}})
			}
			home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
			require.NoError(t, controllerutil.SetControllerReference(workspace, home, scheme))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(workspace).WithObjects(parent, workspace, home, pod).Build()
			gate := repositoryaccess.Gate{Client: c}
			admission, err := gate.Acquire(ctx, parent, repositoryaccess.Token("workspace", workspace), repositoryaccess.Mount, true)
			require.NoError(t, err)
			require.Equal(t, repositoryaccess.Admitted, admission)
			r := WorkspaceReconciler{Client: c, APIReader: c, Scheme: scheme, RunnerImage: testRunnerImage}
			key := client.ObjectKeyFromObject(workspace)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			busy, err := gate.Busy(ctx, parent, "sync")
			require.NoError(t, err)
			require.True(t, busy, "a present Pod must retain the parent reservation")
			require.NoError(t, c.Delete(ctx, pod))

			// The old dependency checks returned before reservation cleanup. A missing
			// dependency then blocked sync even after the runtime Pod disappeared.
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			require.NoError(t, err)
			require.NoError(t, c.Get(ctx, key, workspace))
			require.Equal(t, wantReason, meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady).Reason)
			busy, err = gate.Busy(ctx, parent, "sync")
			require.NoError(t, err)
			require.False(t, busy, "an absent Pod must release its reservation despite unavailable dependencies")
			admission, err = gate.Acquire(ctx, parent, "sync", repositoryaccess.Write, true)
			require.NoError(t, err)
			require.Equal(t, repositoryaccess.Admitted, admission, "sync can proceed without deleting the Workspace")
		})
	}
}
