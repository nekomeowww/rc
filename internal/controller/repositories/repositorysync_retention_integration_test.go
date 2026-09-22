//go:build integration

package repositories

import (
	"context"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("RepositorySync retention", func() {
	It("defaults to three days and rejects negative retention", func() {
		ctx := context.Background()
		request := &repositoriesv1alpha1.RepositorySync{ObjectMeta: metav1.ObjectMeta{Name: "sync-retention-default", Namespace: testNamespace}, Spec: repositoriesv1alpha1.RepositorySyncSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "retention-source"}}}
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, request)).To(Succeed()) })
		Expect(request.Spec.TTLSecondsAfterFinished).To(HaveValue(Equal(int32(259200))))
		invalid := &repositoriesv1alpha1.RepositorySync{ObjectMeta: metav1.ObjectMeta{Name: "sync-retention-negative", Namespace: testNamespace}, Spec: repositoriesv1alpha1.RepositorySyncSpec{RepositoryRef: request.Spec.RepositoryRef, TTLSecondsAfterFinished: new(int32(-1))}}
		Expect(apierrors.IsInvalid(k8sClient.Create(ctx, invalid))).To(BeTrue())
	})
	It("deletes an expired request after removing its access finalizer", func() {
		ctx := context.Background()
		request := &repositoriesv1alpha1.RepositorySync{ObjectMeta: metav1.ObjectMeta{Name: "sync-retention-zero", Namespace: testNamespace, Finalizers: []string{repositoryOperationFinalizer}}, Spec: repositoriesv1alpha1.RepositorySyncSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "retention-source"}, TTLSecondsAfterFinished: new(int32(0))}}
		Expect(k8sClient.Create(ctx, request)).To(Succeed())
		now := metav1.Now()
		request.Status.CompletedAt = &now
		request.Status.Conditions = []metav1.Condition{{Type: repositoriesv1alpha1.RepositorySyncConditionSucceeded, Status: metav1.ConditionFalse, Reason: "RepositoryNotFound", Message: "Repository does not exist", LastTransitionTime: now, ObservedGeneration: request.Generation}}
		Expect(k8sClient.Status().Update(ctx, request)).To(Succeed())
		r := RepositorySyncReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: syncTestRunnerImage}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(request)})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(request), new(repositoriesv1alpha1.RepositorySync)))).To(BeTrue())
	})
})
