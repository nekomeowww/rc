package workspaces

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var testPods = corev1.Resource(string(corev1.ResourcePods))

const testQuotaMessage = "exceeded quota: compute, requested: limits.cpu=4, used: limits.cpu=6, limited: limits.cpu=8"

type admissionFixture struct {
	ctx        context.Context
	client     client.WithWatch
	reconciler *WorkspaceReconciler
	workspace  *workspacesv1alpha1.Workspace
	now        time.Time
	// reject, when set, is returned for creates of matching objects.
	reject  func(client.Object) error
	creates int
}

func newAdmissionFixture(t *testing.T, homeBound bool) *admissionFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, rbacv1.AddToScheme(scheme))
	require.NoError(t, repositoriesv1alpha1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "quota",
			Namespace:  testNamespace,
			UID:        "quota-workspace-uid",
			Finalizers: []string{workspaceFinalizer},
		},
		Spec: workspacesv1alpha1.WorkspaceSpec{
			DesiredState: workspacesv1alpha1.WorkspaceDesiredStateRunning,
			Image:        testRuntimeImage,
			Storage: &workspacesv1alpha1.PersistentStorageSpec{
				StorageClassName: testStorageClass,
				Size:             resource.MustParse("1Gi"),
			},
		},
	}
	objects := []client.Object{workspace}
	if homeBound {
		objects = append(objects, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:            workspace.Name,
				Namespace:       workspace.Namespace,
				OwnerReferences: []metav1.OwnerReference{{UID: workspace.UID, Controller: boolPointer(true)}},
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		})
	}
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, &corev1.PersistentVolumeClaim{}, &corev1.Pod{}, &workspacesv1alpha1.WorkspaceExec{}).
		WithObjects(objects...).
		WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).
		Build()
	fixture := &admissionFixture{
		ctx:       context.Background(),
		client:    base,
		workspace: workspace,
		now:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	kubeClient := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if fixture.reject != nil {
				if err := fixture.reject(obj); err != nil {
					fixture.creates++
					return err
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	fixture.reconciler = &WorkspaceReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
	fixture.reconciler.admissionRetries.now = func() time.Time { return fixture.now }
	return fixture
}

func (fixture *admissionFixture) reconcile(t *testing.T) reconcile.Result {
	t.Helper()
	result, err := fixture.reconciler.Reconcile(fixture.ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(fixture.workspace)})
	require.NoError(t, err)
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), fixture.workspace))
	return result
}

func (fixture *admissionFixture) ready(t *testing.T) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	require.NotNil(t, condition)
	return condition
}

func rejectKind[T client.Object](groupResource schema.GroupResource, cause error) func(client.Object) error {
	return func(obj client.Object) error {
		if _, matches := obj.(T); !matches {
			return nil
		}
		return apierrors.NewForbidden(groupResource, obj.GetName(), cause)
	}
}

// ROOT CAUSE:
// A quota or admission policy refusing the runtime Pod used to surface only as
// a reconcile error, so the Workspace looked like it was still starting and a
// caller waiting for Ready waited without any explanation.
func TestRuntimePodAdmissionRejectionIsPublishedAndRetried(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	fixture.reject = rejectKind[*corev1.Pod](testPods, errors.New(testQuotaMessage))

	result := fixture.reconcile(t)

	ready := fixture.ready(t)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonAdmissionRejected, ready.Reason)
	assert.Contains(t, ready.Message, "Workspace runtime Pod was rejected by the API server")
	assert.Contains(t, ready.Message, testQuotaMessage, "the message names the quota and the exceeded resource")
	assert.Equal(t, fixture.workspace.Generation, ready.ObservedGeneration)
	assert.Equal(t, fixture.workspace.Generation, fixture.workspace.Status.ObservedGeneration)
	assert.Equal(t, admissionRetryInitial, result.RequeueAfter)
	assert.Equal(t, 1, fixture.creates)
	assert.Empty(t, fixture.workspace.Status.RuntimePodName, "no runtime Pod was created")

	// An unrelated event inside the backoff does not repeat the create.
	fixture.now = fixture.now.Add(2 * time.Second)
	result = fixture.reconcile(t)
	assert.Equal(t, 1, fixture.creates)
	assert.Equal(t, admissionRetryInitial-2*time.Second, result.RequeueAfter)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonAdmissionRejected, fixture.ready(t).Reason)

	// A still-exhausted quota doubles the wait, up to the bound.
	fixture.now = fixture.now.Add(admissionRetryInitial)
	result = fixture.reconcile(t)
	assert.Equal(t, 2, fixture.creates)
	assert.Equal(t, 2*admissionRetryInitial, result.RequeueAfter)
	for range 6 {
		fixture.now = fixture.now.Add(result.RequeueAfter)
		result = fixture.reconcile(t)
	}
	assert.Equal(t, admissionRetryMax, result.RequeueAfter)

	// Once the quota frees up, the next retry creates the Pod without user action.
	fixture.reject = nil
	fixture.now = fixture.now.Add(result.RequeueAfter)
	fixture.reconcile(t)

	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), new(corev1.Pod)))
	ready = fixture.ready(t)
	assert.Equal(t, reasonStarting, ready.Reason, "a successful create clears AdmissionRejected")
	_, pending := fixture.reconciler.admissionRetries.pending(admissionRetryKey(fixture.workspace, admissionObjectRuntimePod))
	assert.False(t, pending)
}

func TestRuntimePodAdmissionRejectionRetriesAtOnceAfterSpecChange(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	fixture.reject = rejectKind[*corev1.Pod](testPods, errors.New(`violates PodSecurity "restricted:latest"`))
	fixture.reconcile(t)
	require.Equal(t, 1, fixture.creates)

	fixture.workspace.Spec.DefaultWorkingDirectory = "/workspace/code"
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.workspace))
	fixture.workspace.Generation++ // The fake client does not advance generation.
	require.NoError(t, fixture.client.Update(fixture.ctx, fixture.workspace))
	fixture.reject = nil
	fixture.reconcile(t)

	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), new(corev1.Pod)), "a new generation is not held back by the old backoff")
}

func TestRuntimePodServerErrorStaysReconcileError(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	fixture.reject = func(obj client.Object) error {
		if _, isPod := obj.(*corev1.Pod); !isPod {
			return nil
		}
		return apierrors.NewInternalError(errors.New("failed calling webhook: connection refused"))
	}

	_, err := fixture.reconciler.Reconcile(fixture.ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(fixture.workspace)})

	require.Error(t, err, "a server fault retries through the controller rate limiter")
	require.NoError(t, fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.workspace), fixture.workspace))
	if ready := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady); ready != nil {
		assert.NotEqual(t, workspacesv1alpha1.WorkspaceReasonAdmissionRejected, ready.Reason)
	}
}

func TestHomeVolumeClaimAdmissionRejectionIsPublished(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, false)
	const storageQuota = "exceeded quota: storage, requested: requests.storage=1Gi, used: requests.storage=10Gi, limited: requests.storage=10Gi"
	fixture.reject = rejectKind[*corev1.PersistentVolumeClaim](corev1.Resource(string(corev1.ResourcePersistentVolumeClaims)), errors.New(storageQuota))

	result := fixture.reconcile(t)

	ready := fixture.ready(t)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonAdmissionRejected, ready.Reason)
	assert.Contains(t, ready.Message, storageQuota)
	storage := meta.FindStatusCondition(fixture.workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionStorageReady)
	require.NotNil(t, storage)
	assert.Equal(t, metav1.ConditionFalse, storage.Status)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonAdmissionRejected, storage.Reason)
	assert.Equal(t, admissionRetryInitial, result.RequeueAfter)
	assert.Empty(t, fixture.workspace.Status.HomeVolumeClaimName, "a rejected home is not recorded, so it is not reported as lost later")

	fixture.reject = nil
	fixture.now = fixture.now.Add(result.RequeueAfter)
	fixture.reconcile(t)

	assert.Equal(t, "Provisioning", fixture.ready(t).Reason)
	require.NotEmpty(t, fixture.workspace.Status.HomeVolumeClaimName)
	home := client.ObjectKey{Name: fixture.workspace.Status.HomeVolumeClaimName, Namespace: fixture.workspace.Namespace}
	require.NoError(t, fixture.client.Get(fixture.ctx, home, new(corev1.PersistentVolumeClaim)))
}

func TestHotMountHelperAdmissionRejectionIsReported(t *testing.T) {
	t.Parallel()
	fixture := newAdmissionFixture(t, true)
	runtimePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fixture.workspace.Name, Namespace: fixture.workspace.Namespace, UID: types.UID("runtime-uid")},
		Spec:       corev1.PodSpec{NodeName: "admission-node"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	fixture.reject = rejectKind[*corev1.Pod](testPods, errors.New(`violates PodSecurity "baseline:latest": privileged`))
	const mountName = "rejected"
	mount := hotWorktreeMount{name: mountName, worktree: mountName, path: mountName, claimName: mountName}

	ready, reason, message, err := fixture.reconciler.reconcileHotMounts(fixture.ctx, fixture.workspace, runtimePod, []hotWorktreeMount{mount}, false)

	require.NoError(t, err)
	assert.False(t, ready)
	assert.Equal(t, workspacesv1alpha1.WorkspaceReasonAdmissionRejected, reason)
	assert.Contains(t, message, "Hot Worktree mount rejected Pod was rejected by the API server")
	assert.Contains(t, message, "privileged")
}

func TestAdmissionRejectedClassifiesCreateErrors(t *testing.T) {
	t.Parallel()
	pods := testPods
	terminating := apierrors.NewForbidden(pods, "runtime", errors.New("namespace is being terminated"))
	terminating.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: corev1.NamespaceTerminatingCause}}
	for _, testCase := range []struct {
		name     string
		err      error
		rejected bool
	}{
		{name: "quota", err: apierrors.NewForbidden(pods, "runtime", errors.New(testQuotaMessage)), rejected: true},
		{name: "policy reported as invalid", err: apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "runtime", field.ErrorList{field.Forbidden(field.NewPath("spec"), "denied by policy")}), rejected: true},
		{name: "namespace terminating", err: terminating, rejected: false},
		{name: "webhook unreachable", err: apierrors.NewInternalError(errors.New("failed calling webhook")), rejected: false},
		{name: "server timeout", err: apierrors.NewServerTimeout(pods, "create", 1), rejected: false},
		{name: "throttled", err: apierrors.NewTooManyRequests("slow down", 1), rejected: false},
		{name: "already exists", err: apierrors.NewAlreadyExists(pods, "runtime"), rejected: false},
	} {
		assert.Equal(t, testCase.rejected, admissionRejected(testCase.err), testCase.name)
	}
}

func TestAdmissionRejectionMessageIsBounded(t *testing.T) {
	t.Parallel()
	err := apierrors.NewForbidden(testPods, "runtime", errors.New(strings.Repeat("é", 4*admissionMessageLimit)))

	message := admissionRejectionMessage("Workspace runtime Pod", err)

	assert.Len(t, []rune(message), admissionMessageLimit)
	assert.True(t, strings.HasSuffix(message, "..."))
}
