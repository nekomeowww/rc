package worktreestorage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// Shared source and expansion observations used throughout the policy matrix.
const (
	sourceSize   = "60Gi"
	roundedSize  = "70Gi"
	expandedSize = "80Gi"
	sourceClass  = "source-csi"
)

func TestPlanClone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                              string
		request, capacity, repositorySize string
		modes                             []corev1.PersistentVolumeAccessMode
		override                          *repositoriesv1alpha1.WorktreeStorageSpec
		wantSize, wantClass, wantReason   string
		wantModes                         []corev1.PersistentVolumeAccessMode
	}{
		{name: "RWO source", wantSize: sourceSize},
		{name: "RWX source", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, wantSize: sourceSize},
		{name: "RWOP source", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}, wantSize: sourceSize},
		{name: "stale Repository size", repositorySize: "50Gi", wantSize: sourceSize},
		{name: "larger Repository default", repositorySize: expandedSize, wantSize: expandedSize},
		{name: "capacity exceeds request", request: "50Gi", capacity: roundedSize, wantSize: roundedSize},
		{name: "expansion request exceeds capacity", request: expandedSize, capacity: sourceSize, wantSize: expandedSize},
		{name: "explicit 50Gi cannot clone 60Gi", override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity("50Gi")}, wantReason: SizeTooSmall},
		{name: "explicit size must cover capacity", capacity: expandedSize, override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity(roundedSize)}, wantReason: SizeTooSmall},
		{name: "explicit size must cover expansion request", request: expandedSize, override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity(roundedSize)}, wantReason: SizeTooSmall},
		{name: "zero explicit size", override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity("0")}, wantReason: SizeTooSmall},
		{name: "negative explicit size", override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity("-1Gi")}, wantReason: SizeTooSmall},
		{name: "equal explicit size", override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity(sourceSize)}, wantSize: sourceSize},
		{name: "larger explicit size", override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity("90Gi")}, wantSize: "90Gi"},
		{name: "Repository intent is not the explicit size floor", repositorySize: "90Gi", override: &repositoriesv1alpha1.WorktreeStorageSpec{Size: quantity(roundedSize)}, wantSize: roundedSize},
		{name: "actual source class wins over Repository intent", wantClass: sourceClass, wantSize: sourceSize},
		{name: "different explicit StorageClass", override: &repositoriesv1alpha1.WorktreeStorageSpec{StorageClassName: "other-csi"}, wantClass: "other-csi", wantSize: sourceSize},
		{name: "explicit RWX on capable driver", override: &repositoriesv1alpha1.WorktreeStorageSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}}, wantModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, wantSize: sourceSize},
		{name: "explicit RWO on RWX source", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, override: &repositoriesv1alpha1.WorktreeStorageSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}, wantModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, wantSize: sourceSize},
		{name: "multiple writable modes", modes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteMany}, wantSize: sourceSize},
		{name: "unknown explicit mode", override: &repositoriesv1alpha1.WorktreeStorageSpec{AccessModes: []corev1.PersistentVolumeAccessMode{"unknown"}}, wantReason: StorageInvalid},
		{name: "read only cannot initialize checkout", override: &repositoriesv1alpha1.WorktreeStorageSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}}, wantReason: StorageInvalid},
		{name: "RWOP cannot combine with RWO", override: &repositoriesv1alpha1.WorktreeStorageSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod, corev1.ReadWriteOnce}}, wantReason: StorageInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source, repository := planFixture()
			if tt.request != "" {
				source.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(tt.request)
			}
			if tt.capacity != "" {
				source.Status.Capacity[corev1.ResourceStorage] = resource.MustParse(tt.capacity)
			}
			if tt.repositorySize != "" {
				repository.Spec.Storage.Size = resource.MustParse(tt.repositorySize)
			}
			if tt.modes != nil {
				source.Spec.AccessModes = tt.modes
			}
			sourceBefore, repositoryBefore, overrideBefore := source.DeepCopy(), repository.DeepCopy(), tt.override.DeepCopy()
			plan, err := PlanClone(source, repository, tt.override)
			if tt.wantReason != "" {
				require.NotNil(t, err)
				assert.Equal(t, tt.wantReason, err.Reason)
				assert.NotEmpty(t, err.Error())
				assert.False(t, err.Retryable())
				assert.Equal(t, Plan{}, plan, "failed plans must not expose a usable partial PVC spec")
			} else {
				require.Nil(t, err)
				assert.Zero(t, plan.Size.Cmp(resource.MustParse(tt.wantSize)))
				wantClass := tt.wantClass
				if wantClass == "" {
					wantClass = sourceClass
				}
				assert.Equal(t, wantClass, plan.StorageClassName)
				wantModes := tt.wantModes
				if wantModes == nil {
					wantModes = source.Spec.AccessModes
				}
				assert.Equal(t, wantModes, plan.AccessModes)
				// Returned state is independently owned; consumers may build a PVC from it.
				plan.AccessModes[0] = corev1.ReadOnlyMany
				plan.Size.Add(resource.MustParse("1Gi"))
			}
			assert.Equal(t, sourceBefore, source)
			assert.Equal(t, repositoryBefore, repository)
			assert.Equal(t, overrideBefore, tt.override)
		})
	}
}

func TestPlanCloneRequiresObservedSource(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*corev1.PersistentVolumeClaim)
		reason string
	}{
		{name: "missing status", change: func(p *corev1.PersistentVolumeClaim) { p.Status = corev1.PersistentVolumeClaimStatus{} }, reason: SourceNotReady},
		{name: "pending source", change: func(p *corev1.PersistentVolumeClaim) { p.Status.Phase = corev1.ClaimPending }, reason: SourceNotReady},
		{name: "missing capacity", change: func(p *corev1.PersistentVolumeClaim) { p.Status.Capacity = nil }, reason: SourceNotReady},
		{name: "missing request", change: func(p *corev1.PersistentVolumeClaim) { p.Spec.Resources.Requests = nil }, reason: SourceNotReady},
		{name: "zero capacity", change: func(p *corev1.PersistentVolumeClaim) {
			p.Status.Capacity[corev1.ResourceStorage] = resource.MustParse("0")
		}, reason: SourceNotReady},
		{name: "deleting source", change: func(p *corev1.PersistentVolumeClaim) { now := metav1.Now(); p.DeletionTimestamp = &now }, reason: SourceNotReady},
		{name: "missing class", change: func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = nil }, reason: StorageInvalid},
		{name: "empty class", change: func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = new(string) }, reason: StorageInvalid},
		{name: "missing access modes", change: func(p *corev1.PersistentVolumeClaim) { p.Spec.AccessModes = nil }, reason: StorageInvalid},
		{name: "block source cannot host Git", change: func(p *corev1.PersistentVolumeClaim) { mode := corev1.PersistentVolumeBlock; p.Spec.VolumeMode = &mode }, reason: StorageInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source, repository := planFixture()
			tt.change(source)
			plan, err := PlanClone(source, repository, nil)
			require.NotNil(t, err)
			assert.Equal(t, tt.reason, err.Reason)
			assert.Equal(t, tt.reason == SourceNotReady, err.Retryable())
			assert.Equal(t, !err.Retryable(), err.Reason == SizeTooSmall || err.Reason == StorageInvalid, "Worktree waiters treat every non-retryable reason as terminal")
			assert.Equal(t, Plan{}, plan)
		})
	}
	_, repository := planFixture()
	_, err := PlanClone(nil, repository, nil)
	require.NotNil(t, err)
	assert.True(t, err.Retryable())
}

func planFixture() (*corev1.PersistentVolumeClaim, *repositoriesv1alpha1.Repository) {
	class := sourceClass
	return &corev1.PersistentVolumeClaim{
		Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &class, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(sourceSize)}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(sourceSize)}},
	}, &repositoriesv1alpha1.Repository{Spec: repositoriesv1alpha1.RepositorySpec{Storage: repositoriesv1alpha1.RepositoryStorageSpec{Size: resource.MustParse(sourceSize), StorageClassName: "repository-intent"}}}
}

func quantity(value string) *resource.Quantity {
	result := resource.MustParse(value)
	return &result
}
