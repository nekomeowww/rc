package workspaces

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	testUpgradedImage       = "ghcr.io/example/rc/runner:upgraded"
	testUpgradedRunnerImage = "ghcr.io/example/rc/runner:next"
)

// runtimeContainerImage returns the image of the rc-kube container of the
// Workspace runtime Pod.
func (fixture *admissionFixture) runtimeContainerImage(t *testing.T) string {
	t.Helper()
	pod := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), pod))
	for _, container := range pod.Spec.Containers {
		if container.Name == runtimeContainerName {
			return container.Image
		}
	}
	require.Fail(t, "the runtime Pod has no rc-kube container")
	return ""
}

func (fixture *admissionFixture) outdated(t *testing.T) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionOutdated)
	require.NotNil(t, condition)
	return condition
}

func (fixture *admissionFixture) update(t *testing.T, change func(*workspacesv1alpha1.Workspace)) {
	t.Helper()
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), fixture.workspace))
	change(fixture.workspace)
	fixture.workspace.Generation++ // The fake client does not advance generation.
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.workspace))
}

// suspend stops the runtime Pod and confirms the suspension.
func (fixture *admissionFixture) suspend(t *testing.T) {
	t.Helper()
	fixture.update(t, func(workspace *workspacesv1alpha1.Workspace) {
		workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateSuspended
	})
	fixture.reconcile(t)
	fixture.reconcile(t)
	err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), new(corev1.Pod))
	require.True(t, apierrors.IsNotFound(err), "the suspended Workspace has no runtime Pod")
	require.Equal(t, reasonSuspended, fixture.ready(t).Reason)
}

func (fixture *admissionFixture) start(t *testing.T) {
	t.Helper()
	fixture.update(t, func(workspace *workspacesv1alpha1.Workspace) {
		workspace.Spec.DesiredState = workspacesv1alpha1.WorkspaceDesiredStateRunning
	})
	fixture.reconcile(t)
}

func TestWorkspaceImageChangeWaitsForTheNextStart(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	fixture.reconcile(t)
	require.Equal(t, testRuntimeImage, fixture.runtimeContainerImage(t))
	require.Equal(t, testRuntimeImage, fixture.workspace.Status.RuntimeImage)
	pod := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), pod))

	fixture.update(t, func(workspace *workspacesv1alpha1.Workspace) {
		workspace.Spec.Image = testUpgradedImage
	})
	fixture.reconcile(t)

	running := new(corev1.Pod)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), running), "an image change does not replace the running Pod")
	assert.Equal(t, pod.UID, running.UID)
	assert.Equal(t, testRuntimeImage, fixture.runtimeContainerImage(t))
	assert.Equal(t, testRuntimeImage, fixture.workspace.Status.RuntimeImage, "status keeps the image of the running Pod")
	outdated := fixture.outdated(t)
	assert.Equal(t, metav1.ConditionTrue, outdated.Status)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonImageChangePending, outdated.Reason)
	assert.Contains(t, outdated.Message, testUpgradedImage)

	fixture.suspend(t)
	assert.Equal(t, testRuntimeImage, fixture.workspace.Status.RuntimeImage, "a suspended Workspace records the change only when it starts")
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonImageChangePending, fixture.outdated(t).Reason)

	fixture.start(t)
	assert.Equal(t, testUpgradedImage, fixture.runtimeContainerImage(t), "the next runtime Pod uses the new image")
	assert.Equal(t, testUpgradedImage, fixture.workspace.Status.RuntimeImage)
	outdated = fixture.outdated(t)
	assert.Equal(t, metav1.ConditionFalse, outdated.Status)
	assert.Equal(t, "Current", outdated.Reason)

	// Rollback is the same operation in the other direction.
	fixture.suspend(t)
	fixture.update(t, func(workspace *workspacesv1alpha1.Workspace) {
		workspace.Spec.Image = testRuntimeImage
	})
	fixture.start(t)
	assert.Equal(t, testRuntimeImage, fixture.runtimeContainerImage(t))
	assert.Equal(t, testRuntimeImage, fixture.workspace.Status.RuntimeImage)
}

func TestWorkspaceWithoutImageFollowsTheBaseRunnerImage(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	fixture.update(t, func(workspace *workspacesv1alpha1.Workspace) {
		workspace.Spec.Image = ""
	})
	fixture.reconcile(t)
	require.Equal(t, testRunnerImage, fixture.runtimeContainerImage(t))
	require.Equal(t, testRunnerImage, fixture.workspace.Status.RuntimeImage)

	// A controller upgrade changes --runner-image.
	fixture.reconciler.RunnerImage = testUpgradedRunnerImage
	fixture.reconcile(t)
	assert.Equal(t, testRunnerImage, fixture.runtimeContainerImage(t), "a new base image does not replace the running Pod")
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonImageChangePending, fixture.outdated(t).Reason)

	fixture.suspend(t)
	fixture.start(t)
	assert.Equal(t, testUpgradedRunnerImage, fixture.runtimeContainerImage(t))
	assert.Equal(t, testUpgradedRunnerImage, fixture.workspace.Status.RuntimeImage)
	assert.Equal(t, metav1.ConditionFalse, fixture.outdated(t).Status)
}

func TestWorkspaceImageChangeAppliesToAReplacementPod(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	fixture.reconcile(t)
	fixture.update(t, func(workspace *workspacesv1alpha1.Workspace) {
		workspace.Spec.Image = testUpgradedImage
	})
	fixture.reconcile(t)
	require.Equal(t, testRuntimeImage, fixture.runtimeContainerImage(t))

	// A Pod that is gone for another reason, such as an eviction, is
	// replaced on the desired image.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fixture.workspace.Name, Namespace: fixture.workspace.Namespace}}
	require.NoError(t, fixture.client.Delete(fixture.ctx, pod))
	fixture.reconcile(t)
	assert.Equal(t, testUpgradedImage, fixture.runtimeContainerImage(t))
	assert.Equal(t, testUpgradedImage, fixture.workspace.Status.RuntimeImage)
}

func TestEnvironmentWorkspaceKeepsItsCapturedImage(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme))
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme))
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "prepared", Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceEnvironmentSpec{
			Image: testUpgradedImage,
			Storage: workspacesv1alpha1.PersistentStorageSpec{
				StorageClassName: testStorageClass,
				Size:             resource.MustParse("1Gi"),
			},
		},
		Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{
			CurrentRevision:        2,
			CurrentImage:           testUpgradedImage,
			CurrentVolumeClaimName: "prepared-2",
			Conditions: []metav1.Condition{
				{
					Type:   workspacesv1alpha1.WorkspaceEnvironmentConditionReady,
					Status: metav1.ConditionTrue,
					Reason: testWorkspaceReadyReason,
				},
			},
		},
	}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "cloned", Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			EnvironmentRef: &workspacesv1alpha1.LocalReference{Name: environment.Name},
			Image:          testUpgradedImage,
		},
		Status: workspacesv1alpha1.WorkspaceStatus{
			RuntimeImage:              testRuntimeImage,
			SourceEnvironmentRevision: 1,
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment, workspace).Build()
	reconciler := &WorkspaceReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme, RunnerImage: testUpgradedRunnerImage}

	resolved, reason, _, err := reconciler.resolveWorkspaceBase(context.Background(), workspace)
	requirements.NoError(err)
	requirements.Empty(reason)
	assert.Equal(t, testRuntimeImage, resolved.image, "an Environment clone keeps the image captured at creation")
	assert.Empty(t, resolved.desiredImage, "spec.image and the base image do not apply to an Environment clone")
	assert.False(t, resolved.imageChangePending())
	assert.True(t, resolved.outdated, "the newer Environment image is reported as Outdated")
}
