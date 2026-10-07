package worktreestorage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCloneSourceName(t *testing.T) {
	t.Parallel()
	const expectedSourceName = "source"
	emptyGroup := ""
	otherGroup := "custom.example"
	namespace := metav1.NamespaceDefault
	otherNamespace := "other-namespace"
	for _, tt := range []struct {
		name      string
		source    *corev1.TypedLocalObjectReference
		sourceRef *corev1.TypedObjectReference
		wantError bool
	}{
		{name: "legacy", source: &corev1.TypedLocalObjectReference{Kind: cloneSourceKind, Name: expectedSourceName}},
		{name: "modern", sourceRef: &corev1.TypedObjectReference{Kind: cloneSourceKind, Name: expectedSourceName}},
		{name: "both normalized", source: &corev1.TypedLocalObjectReference{Kind: cloneSourceKind, Name: expectedSourceName}, sourceRef: &corev1.TypedObjectReference{APIGroup: &emptyGroup, Kind: cloneSourceKind, Name: expectedSourceName}},
		{name: "explicit local namespace", sourceRef: &corev1.TypedObjectReference{Kind: cloneSourceKind, Name: expectedSourceName, Namespace: &namespace}},
		{name: "empty namespace", sourceRef: &corev1.TypedObjectReference{Kind: cloneSourceKind, Name: expectedSourceName, Namespace: &emptyGroup}},
		{name: "missing", wantError: true},
		{name: "empty legacy name", source: &corev1.TypedLocalObjectReference{Kind: cloneSourceKind}, wantError: true},
		{name: "empty modern name", sourceRef: &corev1.TypedObjectReference{Kind: cloneSourceKind}, wantError: true},
		{name: "wrong legacy kind", source: &corev1.TypedLocalObjectReference{Kind: "VolumeSnapshot", Name: expectedSourceName}, wantError: true},
		{name: "wrong modern kind", sourceRef: &corev1.TypedObjectReference{Kind: "VolumeSnapshot", Name: expectedSourceName}, wantError: true},
		{name: "wrong legacy group", source: &corev1.TypedLocalObjectReference{APIGroup: &otherGroup, Kind: cloneSourceKind, Name: expectedSourceName}, wantError: true},
		{name: "wrong modern group", sourceRef: &corev1.TypedObjectReference{APIGroup: &otherGroup, Kind: cloneSourceKind, Name: expectedSourceName}, wantError: true},
		{name: "cross namespace", sourceRef: &corev1.TypedObjectReference{Kind: cloneSourceKind, Name: expectedSourceName, Namespace: &otherNamespace}, wantError: true},
		{name: "ambiguous", source: &corev1.TypedLocalObjectReference{Kind: cloneSourceKind, Name: expectedSourceName}, sourceRef: &corev1.TypedObjectReference{Kind: cloneSourceKind, Name: "different"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace}, Spec: corev1.PersistentVolumeClaimSpec{DataSource: tt.source, DataSourceRef: tt.sourceRef}}
			before := claim.DeepCopy()
			name, err := CloneSourceName(claim)
			if tt.wantError {
				require.Error(t, err)
				assert.Empty(t, name, "do not return an identity inferred from only one conflicting field")
			} else {
				require.NoError(t, err)
				assert.Equal(t, expectedSourceName, name)
			}
			assert.Equal(t, before, claim, "recovery must not mutate committed PVC fields")
		})
	}
}
