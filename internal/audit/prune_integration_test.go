//go:build integration

package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// TestAPIConditionalDeletion uses an isolated API server. It deliberately runs
// without the garbage collector so accepted orphan deletion stays observable as
// pending instead of falsely reporting convergence from the DELETE response.
func TestAPIConditionalDeletion(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	kube, err := client.NewWithWatch(config, client.Options{Scheme: fixtureClient(t).Scheme()})
	require.NoError(t, err)
	require.NoError(t, kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}))
	target := &workspaces.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testWorkspaceName, Namespace: testNamespace}}
	require.NoError(t, kube.Create(t.Context(), target))
	controller := true
	now := time.Now().UTC().Truncate(time.Second)
	for _, race := range []string{"version", "uid", "none"} {
		t.Run(race, func(t *testing.T) {
			record := &workspaces.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "completed-" + race, Namespace: testNamespace, Finalizers: []string{workspaceExecFinalizer}, OwnerReferences: []metav1.OwnerReference{{APIVersion: workspaceAPI, Kind: workspaceKind, Name: target.Name, UID: target.UID, Controller: &controller}}}, Spec: workspaces.WorkspaceExecSpec{TargetRef: workspaces.WorkspaceExecTargetReference{Kind: workspaces.WorkspaceExecTargetWorkspace, Name: testWorkspaceName}, Command: []string{"true"}}}
			require.NoError(t, kube.Create(t.Context(), record))
			record.Status.Phase = workspaces.WorkspaceExecPhaseSucceeded
			record.Status.CompletedAt = new(metav1.NewTime(now.Add(-30 * 24 * time.Hour)))
			require.NoError(t, kube.Status().Update(t.Context(), record))
			review, err := Review(t.Context(), kube, testNamespace, HistoryPolicy{HistoryFor: DefaultPolicy().HistoryFor}, nil, allowHistory, now)
			require.NoError(t, err)
			// Limit this API race to the current subtest's reviewed identity.
			saved := review.Plan()
			saved.Candidates = []ObjectRef{project(record, workspaceAPI, workspaceExecKind).ObjectRef}
			review, err = Review(t.Context(), kube, testNamespace, saved.Policy, &saved, allowHistory, now)
			require.NoError(t, err)
			// ROOT CAUSE: GET alone cannot protect the subsequent DELETE. Mutate or
			// replace the object after the last GET; the API must enforce both fences.
			racing := interceptor.NewClient(kube, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if race == "version" {
					record.Labels = map[string]string{"race": "update"}
					require.NoError(t, c.Update(ctx, record))
				}
				if race == "uid" {
					require.NoError(t, c.Delete(ctx, record))
					record.ResourceVersion, record.UID = "", ""
					require.NoError(t, c.Create(ctx, record))
				}
				return c.Delete(ctx, obj, opts...)
			}})
			result, err := Prune(t.Context(), racing, review, now)
			if race != "none" {
				require.True(t, apierrors.IsConflict(err), "API must atomically reject stale UID/resourceVersion: %v", err)
				assert.Empty(t, result.Requested)
				require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(record), record))
				return
			}
			require.NoError(t, err)
			assert.Len(t, result.Requested, 1)
			assert.Empty(t, result.Absent)
			assert.Len(t, result.Pending, 1)
			result, err = Prune(t.Context(), kube, review, now)
			require.NoError(t, err)
			assert.Empty(t, result.Requested)
			assert.Len(t, result.Pending, 1)
		})
	}
}
