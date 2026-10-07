package repositoryaccess

import (
	"sync"
	"testing"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testSourceName    = "source"
	testRepositoryUID = "repository"
)

func holder(name string, mode Mode) holdset.Holder {
	return holdset.Holder{Kind: "Test", Name: name, UID: types.UID(name + "-uid"), Mode: mode}
}

func key(name string) string {
	return holder(name, "").Key()
}

func TestAdmissionSerializesWritersAndRetainsReaders(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, repositories.AddToScheme(scheme))
	repository := &repositories.Repository{ObjectMeta: metav1.ObjectMeta{Name: testSourceName, Namespace: metav1.NamespaceDefault, UID: testRepositoryUID, Generation: 1}, Status: repositories.RepositoryStatus{ObservedGeneration: 1, VolumeClaimName: testSourceName, Conditions: []metav1.Condition{{Type: repositories.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()
	gate := Gate{Client: c, Reader: c}
	// Two controllers must not both pass admission before either creates its Job.
	var workers sync.WaitGroup
	results := make(chan string, 2)
	for _, token := range []string{"first", "second"} {
		workers.Go(func() {
			acquired, err := gate.Acquire(t.Context(), repository, holder(token, Write), true)
			if err != nil {
				t.Error(err)
			}
			if acquired == Admitted {
				results <- token
			}
		})
	}
	workers.Wait()
	close(results)
	winners := []string{}
	for token := range results {
		winners = append(winners, token)
	}
	require.Len(t, winners, 1)
	require.NoError(t, gate.Release(t.Context(), repository, key(winners[0])))
	admitted, err := gate.Acquire(t.Context(), repository, holder("clone-one", Clone), true)
	require.NoError(t, err)
	require.Equal(t, Admitted, admitted)
	admitted, err = gate.Acquire(t.Context(), repository, holder("clone-two", Clone), true)
	require.NoError(t, err)
	require.Equal(t, Admitted, admitted)
	admitted, err = gate.Acquire(t.Context(), repository, holder("sync", Write), false)
	require.NoError(t, err)
	require.Equal(t, NotReserved, admitted)
	admitted, err = gate.Acquire(t.Context(), repository, holder("mount", Mount), true)
	require.NoError(t, err)
	require.Equal(t, NotReserved, admitted)
	require.NoError(t, gate.Release(t.Context(), repository, key("clone-one")))
	admitted, err = gate.Acquire(t.Context(), repository, holder("sync", Write), false)
	require.NoError(t, err)
	require.Equal(t, NotReserved, admitted)
	require.NoError(t, gate.Release(t.Context(), repository, key("clone-two")))
	admitted, err = gate.Acquire(t.Context(), repository, holder("sync", Write), false)
	require.NoError(t, err)
	require.Equal(t, Admitted, admitted)
}

func TestAdmissionRejectsStaleReadiness(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, repositories.AddToScheme(scheme))
	repository := &repositories.Repository{ObjectMeta: metav1.ObjectMeta{Name: testSourceName, Namespace: metav1.NamespaceDefault, UID: testRepositoryUID, Generation: 2}, Status: repositories.RepositoryStatus{ObservedGeneration: 1, VolumeClaimName: testSourceName, Conditions: []metav1.Condition{{Type: repositories.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()
	gate := Gate{Client: c, Reader: c}
	acquired, err := gate.Acquire(t.Context(), repository, holder("clone", Clone), true)
	require.NoError(t, err)
	require.Equal(t, NotReserved, acquired)
	busy, err := gate.Busy(t.Context(), repository, key("sync"))
	require.NoError(t, err)
	require.False(t, busy, "stale readiness must not leak a reservation")
}

func TestOldBootstrapCannotPublishOverNewSyncState(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, repositories.AddToScheme(scheme))
	repository := &repositories.Repository{ObjectMeta: metav1.ObjectMeta{Name: testSourceName, Namespace: metav1.NamespaceDefault, UID: testRepositoryUID, Generation: 1}}
	stale := repository.DeepCopy()
	repository.Status.Conditions = []metav1.Condition{{Type: repositories.RepositoryConditionStorageReady, Status: metav1.ConditionFalse, Reason: "SyncFailed", ObservedGeneration: 1}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository).Build()
	gate := Gate{Client: c, Reader: c}
	// A bootstrap reconcile can span an entire sync. Generation alone cannot
	// distinguish its old result from the newer status at the same generation.
	acquired, err := gate.Acquire(t.Context(), stale, holder("bootstrap", Write), false)
	require.NoError(t, err)
	require.Equal(t, NotReserved, acquired)
	busy, err := gate.Busy(t.Context(), repository, key("sync"))
	require.NoError(t, err)
	require.False(t, busy)
}

func TestAdmissionRetainsReservationWhileConsumersStop(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, repositories.AddToScheme(scheme))
	repository := &repositories.Repository{ObjectMeta: metav1.ObjectMeta{Name: testSourceName, Namespace: metav1.NamespaceDefault, UID: testRepositoryUID, Generation: 1}, Status: repositories.RepositoryStatus{ObservedGeneration: 1, VolumeClaimName: testSourceName, Conditions: []metav1.Condition{{Type: repositories.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "earlier-consumer", Namespace: repository.Namespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "parent", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: repository.Status.VolumeClaimName},
		}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, pod).Build()
	gate := Gate{Client: c, Reader: c}
	admission, err := gate.Acquire(t.Context(), repository, holder("sync", Write), true)
	require.NoError(t, err)
	require.Equal(t, Reserved, admission, "a reservation alone does not permit consumer creation")
	busy, err := gate.Busy(t.Context(), repository, key("another-writer"))
	require.NoError(t, err)
	require.True(t, busy)
	admission, err = gate.Acquire(t.Context(), repository, holder("another-writer", Write), true)
	require.NoError(t, err)
	require.Equal(t, NotReserved, admission)
	require.NoError(t, c.Delete(t.Context(), pod))
	admission, err = gate.Acquire(t.Context(), repository, holder("sync", Write), true)
	require.NoError(t, err)
	require.Equal(t, Admitted, admission, "the same reservation admits a retry after consumers stop")
	require.NoError(t, gate.Release(t.Context(), repository, key("sync")))
	busy, err = gate.Busy(t.Context(), repository, key("another-writer"))
	require.NoError(t, err)
	require.False(t, busy)
}

func TestHeldByMatchesAnyKindForUID(t *testing.T) {
	t.Parallel()
	owner := &repositories.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "owner", UID: "owner-uid"}}
	lease := &coordinationv1.Lease{}
	require.NoError(t, holdset.Encode(lease, StateAnnotation, holdset.State{Holders: []holdset.Holder{Holder(KindWorkspace, owner, Mount), {Kind: KindWorktree, Name: "x", UID: "other-uid", Mode: Mount}}}))
	held, err := HeldBy(lease, owner.UID)
	require.NoError(t, err)
	require.True(t, held)
	held, err = HeldBy(lease, "owner")
	require.NoError(t, err)
	require.False(t, held, "UID must match exactly")
	held, err = HeldBy(lease, "")
	require.NoError(t, err)
	require.False(t, held)
	lease.Annotations[StateAnnotation] = "{"
	_, err = HeldBy(lease, owner.UID)
	require.Error(t, err)
}

func TestLegacyReservationBlocksWritersUntilReleasedByKind(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, repositories.AddToScheme(scheme))
	repository := &repositories.Repository{ObjectMeta: metav1.ObjectMeta{Name: testSourceName, Namespace: metav1.NamespaceDefault, UID: testRepositoryUID, Generation: 1}, Status: repositories.RepositoryStatus{ObservedGeneration: 1, VolumeClaimName: testSourceName, Conditions: []metav1.Condition{{Type: repositories.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	workspace := &repositories.Worktree{ObjectMeta: metav1.ObjectMeta{Name: "consumer", UID: "consumer-uid"}}
	// An older controller encoded holders as role/uid/name tokens with one set mode.
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseKey(repository).Name, Namespace: repository.Namespace, Annotations: map[string]string{
		StateAnnotation: `{"mode":"mount","holders":{"workspace/consumer-uid/consumer":true}}`,
	}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repository, lease).Build()
	gate := Gate{Client: c, Reader: c}
	held, err := HeldBy(lease, workspace.UID)
	require.NoError(t, err)
	require.True(t, held)
	admission, err := gate.Acquire(t.Context(), repository, holder("sync", Write), false)
	require.NoError(t, err)
	require.Equal(t, NotReserved, admission, "an upgraded controller honors reservations it did not write")
	require.NoError(t, gate.Release(t.Context(), repository, Holder(KindWorkspace, workspace, Mount).Key()))
	admission, err = gate.Acquire(t.Context(), repository, holder("sync", Write), false)
	require.NoError(t, err)
	require.Equal(t, Admitted, admission)
	current := new(coordinationv1.Lease)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(lease), current))
	state, err := holdset.Decode(current, StateAnnotation)
	require.NoError(t, err, "the first write migrates the encoding")
	require.Len(t, state.Holders, 1)
}
