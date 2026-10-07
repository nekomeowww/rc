package holdset

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testAnnotation = "example.test/holders"
	read           = Mode("read")
	write          = Mode("write")
)

// exclusiveWriter admits any number of readers and at most one writer.
func exclusiveWriter(existing []Holder, candidate Holder) []Holder {
	if candidate.Mode != write {
		return nil
	}
	var blocking []Holder
	for _, holder := range existing {
		if holder.Mode == write {
			blocking = append(blocking, holder)
		}
	}
	return blocking
}

func owner(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID(name + "-uid")}}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	return scheme
}

func annotationStore(c client.Client, target *corev1.ConfigMap) AnnotationStore {
	return AnnotationStore{Client: c, Reader: c, Key: client.ObjectKeyFromObject(target), UID: target.UID, Annotation: testAnnotation,
		New: func() client.Object { return new(corev1.ConfigMap) }, Legacy: []string{"example.test/legacy"}}
}

func TestAcquireLosesCASRaceToClose(t *testing.T) {
	target := owner("target")
	injected := false
	var store AnnotationStore
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(target).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if !injected {
				injected = true
				// The fence commits between the admission read and its write.
				remaining, err := Close(ctx, AnnotationStore{Client: c, Reader: c, Key: store.Key, UID: store.UID, Annotation: testAnnotation, New: store.New})
				require.NoError(t, err)
				require.Empty(t, remaining)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	store = annotationStore(kube, target)
	result, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner("consumer"), read), exclusiveWriter)
	require.NoError(t, err)
	assert.True(t, injected)
	assert.False(t, result.Admitted)
	assert.True(t, result.Closed)
	_, state, err := store.Load(t.Context())
	require.NoError(t, err)
	assert.True(t, state.Closed)
	assert.Empty(t, state.Holders)
}

func TestAdmittedHolderSurvivesCloseUntilRelease(t *testing.T) {
	target := owner("target")
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(target).Build()
	store := annotationStore(kube, target)
	consumer := HolderFor("ConfigMap", owner("consumer"), write)
	result, err := Acquire(t.Context(), store, consumer, exclusiveWriter)
	require.NoError(t, err)
	require.True(t, result.Admitted)
	// No consumer is visible yet. Closing must still wait for its admitted creator.
	remaining, err := Close(t.Context(), store)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	result, err = Acquire(t.Context(), store, consumer, exclusiveWriter)
	require.NoError(t, err)
	assert.False(t, result.Admitted, "a closed set admits nothing new")
	assert.True(t, result.Held, "the existing holder still holds its mode")
	replacement := consumer
	replacement.UID = "replacement-uid"
	require.NoError(t, Release(t.Context(), store, replacement.Key()))
	remaining, err = Close(t.Context(), store)
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "a same-name replacement cannot release the old incarnation")
	require.NoError(t, Release(t.Context(), store, consumer.Key()))
	remaining, err = Close(t.Context(), store)
	require.NoError(t, err)
	assert.Empty(t, remaining)
	result, err = Acquire(t.Context(), store, HolderFor("ConfigMap", owner("late"), read), exclusiveWriter)
	require.NoError(t, err)
	assert.False(t, result.Admitted, "releasing the last holder never reopens the fence")
}

func TestStoreTreatsAnotherIncarnationAsAbsent(t *testing.T) {
	target := owner("target")
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(target).Build()
	stale := annotationStore(kube, target)
	stale.UID = "previous-uid"
	result, err := Acquire(t.Context(), stale, HolderFor("ConfigMap", owner("consumer"), read), exclusiveWriter)
	require.NoError(t, err)
	assert.False(t, result.Admitted)
	require.NoError(t, Release(t.Context(), stale, "ConfigMap/consumer-uid"))
	_, err = Close(t.Context(), stale)
	require.ErrorIs(t, err, ErrAbsent)
}

func TestConcurrentWritersAdmitExactlyOne(t *testing.T) {
	scheme := testScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := LeaseStore{Client: kube, Reader: kube, Key: client.ObjectKey{Namespace: "test", Name: "gate"}, Annotation: testAnnotation}
	var workers sync.WaitGroup
	winners := make(chan string, 2)
	first, second := "first", "second"
	for _, name := range []string{first, second} {
		workers.Go(func() {
			result, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner(name), write), exclusiveWriter)
			if err != nil {
				t.Error(err)
			}
			if result.Admitted {
				winners <- name
			}
		})
	}
	workers.Wait()
	close(winners)
	admitted := make([]string, 0, 2)
	for name := range winners {
		admitted = append(admitted, name)
	}
	require.Len(t, admitted, 1, "the Lease create and update race must admit one writer")
	reader, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner("reader"), read), exclusiveWriter)
	require.NoError(t, err)
	assert.True(t, reader.Admitted, "readers coexist with the writer")
	loser := map[string]string{first: second, second: first}[admitted[0]]
	blocked, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner(loser), write), exclusiveWriter)
	require.NoError(t, err)
	require.False(t, blocked.Admitted)
	require.Len(t, blocked.Conflicts, 1)
	assert.Equal(t, admitted[0], blocked.Conflicts[0].Name)
	require.NoError(t, Release(t.Context(), store, HolderFor("ConfigMap", owner(admitted[0]), write).Key()))
	upgraded, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner("reader"), write), exclusiveWriter)
	require.NoError(t, err)
	assert.True(t, upgraded.Admitted, "a reader upgrades in place once the writer leaves")
	_, state, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Len(t, state.Holders, 1)
	assert.Equal(t, write, state.Holders[0].Mode)
}

func TestSweepRemovesOnlyStaleHolders(t *testing.T) {
	target := owner("target")
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(target).Build()
	store := annotationStore(kube, target)
	for _, name := range []string{"gone", "live"} {
		result, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner(name), read), exclusiveWriter)
		require.NoError(t, err)
		require.True(t, result.Admitted)
	}
	removed, err := Sweep(t.Context(), store, func(h Holder) (bool, error) { return h.Name == "gone", nil })
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Equal(t, "gone", removed[0].Name)
	_, state, err := store.Load(t.Context())
	require.NoError(t, err)
	require.Len(t, state.Holders, 1)
	assert.Equal(t, "live", state.Holders[0].Name)
}

func TestSaveDropsLegacyAnnotations(t *testing.T) {
	target := owner("target")
	target.Annotations = map[string]string{"example.test/legacy": "{}", "unrelated": "kept"}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(target).Build()
	store := annotationStore(kube, target)
	_, err := Acquire(t.Context(), store, HolderFor("ConfigMap", owner("consumer"), read), exclusiveWriter)
	require.NoError(t, err)
	current := new(corev1.ConfigMap)
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(target), current))
	assert.NotContains(t, current.Annotations, "example.test/legacy")
	assert.Equal(t, "kept", current.Annotations["unrelated"])
	state, err := Decode(current, testAnnotation)
	require.NoError(t, err)
	require.Len(t, state.Holders, 1)
	assert.False(t, state.Holders[0].Since.IsZero())
}
