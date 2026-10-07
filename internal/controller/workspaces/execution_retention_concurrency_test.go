package workspaces

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testRetentionSiblingUID   = "b-uid"
	raceChangeExtended        = "extend"
	raceChangeDisabled        = "disable"
	raceChangeActive          = "running"
	raceChangeAnnotations     = "metadata"
	testRetentionRaceTarget   = "retention-race-target"
	testRetentionExecResource = "workspaceexecs"
	testPreviousTargetUID     = "previous-target"
)

// retentionFixture supplies fresh API identities without a running runtime.
func retentionFixture(t *testing.T) (client.WithWatch, *workspacesv1alpha1.Workspace, *workspacesv1alpha1.WorkspaceExec) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	ws := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testRetentionRaceTarget, Namespace: testNamespace, UID: "target-uid"}, Spec: workspacesv1alpha1.WorkspaceSpec{ExecutionRetention: &workspacesv1alpha1.ExecutionRetentionPolicy{TTLAfterFinished: &metav1.Duration{Duration: 7 * 24 * time.Hour}}}}
	done := metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour))
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNamespace, UID: "a-uid"}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: executionTargetReference(ws)}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseFailed, CompletedAt: &done}}
	require.NoError(t, controllerutil.SetControllerReference(ws, process, scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, process).WithStatusSubresource(ws, process, &corev1.Pod{}, &workspacesv1alpha1.WorkspaceEnvironment{}).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	return kube, ws, process
}

// ROOT CAUSE: cached selection never authorizes deletion. Core mutations are
// tested once through the shared service; target routing has its own smoke test.
func TestRetentionRevalidatesConcurrentChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"pin", raceChangeActive, "completed", raceChangeDisabled, raceChangeExtended, raceChangeAnnotations} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			base, ws, a := retentionFixture(t)
			b := a.DeepCopy()
			b.Name, b.UID, b.ResourceVersion = "b", testRetentionSiblingUID, ""
			require.NoError(t, base.Create(t.Context(), b))
			changed := false
			kube := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				if changed {
					return nil
				}
				changed = true
				switch change {
				case "pin":
					a.Spec.Retain = true
					return base.Update(ctx, a)
				case raceChangeActive:
					a.Status.Phase = workspacesv1alpha1.WorkspaceExecPhaseRunning
					return base.Status().Update(ctx, a)
				case "completed":
					now := metav1.Now()
					a.Status.CompletedAt = &now
					return base.Status().Update(ctx, a)
				case raceChangeDisabled:
					ws.Spec.ExecutionRetention = nil
					return base.Update(ctx, ws)
				case raceChangeExtended:
					ws.Spec.ExecutionRetention.TTLAfterFinished = &metav1.Duration{Duration: 60 * 24 * time.Hour}
					return base.Update(ctx, ws)
				default:
					a.Annotations = map[string]string{"concurrent": "write"}
					return base.Update(ctx, a)
				}
			}})
			_, err := (&executionRetentionService{Client: kube, APIReader: kube}).reconcileExecutionHistory(t.Context(), ws.Namespace, executionTargetReference(ws), ws.UID, ws.Spec.ExecutionRetention)
			require.NoError(t, err)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(a), a))
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(b), b))
			require.Equal(t, change == raceChangeAnnotations, !a.DeletionTimestamp.IsZero())
			require.Equal(t, change != raceChangeDisabled && change != raceChangeExtended, !b.DeletionTimestamp.IsZero(), "sibling still progresses")
			if change == raceChangeAnnotations {
				require.Equal(t, "write", a.Annotations["concurrent"])
			}
		})
	}
}

func TestRetentionConflictDoesNotAbortBatch(t *testing.T) {
	t.Parallel()
	base, ws, a := retentionFixture(t)
	b := a.DeepCopy()
	b.Name, b.UID, b.ResourceVersion = "b", testRetentionSiblingUID, ""
	require.NoError(t, base.Create(t.Context(), b))
	kube := interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if obj.GetName() == "b" {
			return apierrors.NewConflict(schema.GroupResource{Resource: testRetentionExecResource}, obj.GetName(), errors.New("concurrent pin"))
		}
		return c.Update(ctx, obj, opts...)
	}})
	_, err := (&executionRetentionService{Client: kube, APIReader: kube}).reconcileExecutionHistory(t.Context(), ws.Namespace, executionTargetReference(ws), ws.UID, ws.Spec.ExecutionRetention)
	require.NoError(t, err)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(a), a))
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(b), b))
	require.False(t, a.DeletionTimestamp.IsZero())
	require.True(t, b.DeletionTimestamp.IsZero())
}

// A finalizer installation is a separate write, not permission to delete using
// the old eligibility decision. Also verify that a count-only plan re-reads peers.
func TestRetentionRechecksAfterFinalizerAndCountChanges(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"pin-after-finalizer", "policy-after-finalizer", "pin-count-peer", "delete-conflict"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			scheme := runtime.NewScheme()
			require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
			policy := &workspacesv1alpha1.ExecutionRetentionPolicy{MaxEntries: 1}
			ws := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testRetentionRaceTarget, Namespace: testNamespace, UID: "target-uid"}, Spec: workspacesv1alpha1.WorkspaceSpec{ExecutionRetention: policy}}
			done := metav1.NewTime(time.Now().Add(-time.Hour))
			a := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNamespace, UID: "a"}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: ws.Name}}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseFailed, CompletedAt: &done}}
			require.NoError(t, controllerutil.SetControllerReference(ws, a, scheme))
			b := a.DeepCopy()
			b.Name = "b"
			b.UID = "b"
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, a, b).WithStatusSubresource(a, b).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
			mutated := false
			kube := interceptor.NewClient(base, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}
					if change == "pin-count-peer" && !mutated {
						mutated = true
						require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(a), a))
						a.Spec.Retain = true
						require.NoError(t, base.Update(ctx, a))
					}
					return nil
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}
					if obj.GetName() != "b" || mutated {
						return nil
					}
					mutated = true
					if change == "pin-after-finalizer" {
						require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(b), b))
						b.Spec.Retain = true
						return base.Update(ctx, b)
					}
					if change == "policy-after-finalizer" {
						ws.Spec.ExecutionRetention = nil
						return base.Update(ctx, ws)
					}
					return nil
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if change == "delete-conflict" {
						return apierrors.NewConflict(schema.GroupResource{Resource: testRetentionExecResource}, obj.GetName(), errors.New("concurrent pin"))
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			r := &executionRetentionService{Client: kube, APIReader: kube}
			result, err := r.reconcileExecutionHistory(ctx, ws.Namespace, executionTargetReference(ws), ws.UID, ws.Spec.ExecutionRetention)
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
			require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(b), b))
			require.True(t, b.DeletionTimestamp.IsZero())
		})
	}
}

func TestRetentionUsesAPIReaderInsteadOfStaleCache(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	ws := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testRetentionRaceTarget, Namespace: testNamespace}, Spec: workspacesv1alpha1.WorkspaceSpec{ExecutionRetention: &workspacesv1alpha1.ExecutionRetentionPolicy{TTLAfterFinished: &metav1.Duration{Duration: 7 * 24 * time.Hour}}}}
	done := metav1.NewTime(time.Now().Add(-30 * 24 * time.Hour))
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: testNamespace, UID: "old"}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: ws.Name}}, Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseFailed, CompletedAt: &done}}
	cache := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, process).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	process.Spec.Retain = true
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, process).WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).Build()
	r := &executionRetentionService{Client: cache, APIReader: api}
	_, err := r.reconcileExecutionHistory(ctx, ws.Namespace, executionTargetReference(ws), ws.UID, ws.Spec.ExecutionRetention)
	require.NoError(t, err)
	require.NoError(t, cache.Get(ctx, client.ObjectKeyFromObject(process), process))
	require.True(t, process.DeletionTimestamp.IsZero())
	require.Empty(t, process.Finalizers)
}

func TestRetentionCountIgnoresPreviousTargetIdentity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	kube, ws, current := retentionFixture(t)
	ws.Spec.ExecutionRetention.MaxEntries = 1
	require.NoError(t, kube.Update(ctx, ws))
	now := metav1.Now()
	current.Status.CompletedAt = &now
	require.NoError(t, kube.Status().Update(ctx, current))
	controller := true
	current.OwnerReferences = []metav1.OwnerReference{{APIVersion: workspacesv1alpha1.GroupVersion.String(), Kind: string(workspacesv1alpha1.WorkspaceExecTargetWorkspace), Name: ws.Name, UID: ws.UID, Controller: &controller}}
	require.NoError(t, kube.Update(ctx, current))
	previous := current.DeepCopy()
	previous.Name, previous.UID, previous.ResourceVersion = "previous", "previous-uid", ""
	previous.OwnerReferences[0].UID = testPreviousTargetUID
	newer := metav1.NewTime(now.Add(time.Minute))
	previous.Status.CompletedAt = &newer
	require.NoError(t, kube.Create(ctx, previous))
	_, err := (&executionRetentionService{Client: kube, APIReader: kube}).reconcileExecutionHistory(ctx, ws.Namespace, executionTargetReference(ws), ws.UID, ws.Spec.ExecutionRetention)
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(current), current))
	require.True(t, current.DeletionTimestamp.IsZero(), "an old target's newer result must not evict this target's sole record")
}

func TestRetentionBatchIgnoresDeletingPreviousTargetIdentity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	kube, ws, current := retentionFixture(t)
	controller := true
	current.OwnerReferences = []metav1.OwnerReference{{APIVersion: workspacesv1alpha1.GroupVersion.String(), Kind: string(workspacesv1alpha1.WorkspaceExecTargetWorkspace), Name: ws.Name, UID: ws.UID, Controller: &controller}}
	require.NoError(t, kube.Update(ctx, current))
	for i := range executionCleanupBatch {
		previous := current.DeepCopy()
		previous.Name = fmt.Sprintf("previous-%02d", i)
		previous.UID, previous.ResourceVersion = "", ""
		previous.Finalizers = []string{"test/hold"}
		previous.OwnerReferences[0].UID = testPreviousTargetUID
		require.NoError(t, kube.Create(ctx, previous))
		require.NoError(t, kube.Delete(ctx, previous))
	}

	_, err := (&executionRetentionService{Client: kube, APIReader: kube}).reconcileExecutionHistory(ctx, ws.Namespace, executionTargetReference(ws), ws.UID, ws.Spec.ExecutionRetention)
	require.NoError(t, err)
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(current), current))
	require.False(t, current.DeletionTimestamp.IsZero(), "old target cleanup must not exhaust the current target's batch")
}

func TestRetentionRejectsUnprovenTargetIdentity(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"missing", "stale"} {
		t.Run(owner, func(t *testing.T) {
			kube, _, current := retentionFixture(t)
			if owner == "missing" {
				current.OwnerReferences = nil
			} else {
				current.OwnerReferences[0].UID = testPreviousTargetUID
			}
			require.NoError(t, kube.Update(t.Context(), current))
			candidate, err := (&executionRetentionService{Client: kube, APIReader: kube}).executionCleanupCandidate(t.Context(), current)
			require.NoError(t, err)
			require.Nil(t, candidate, "same-name targets must not adopt history without matching owner identity")
		})
	}
}
