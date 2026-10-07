//go:build integration

package repositories

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var _ = Describe("Worktree PVC watch", func() {
	It("registers the Repository index and wakes a Pending child when its PVC binds", func() {
		// A namespace-scoped manager exercises the real informer/index wiring
		// without reconciling objects owned by the other envtest cases.
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "worktree-watch-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(workspacesv1alpha1.AddToScheme(scheme)).To(Succeed())
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme,
			Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace.Name: {}}},
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())
		r := &WorktreeReconciler{Client: mgr.GetClient(), Scheme: scheme, RunnerImage: syncTestRunnerImage}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
		managerCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- mgr.Start(managerCtx) }()
		DeferCleanup(func() {
			stop()
			Eventually(done, time.Second*10).Should(Receive(Succeed()))
		})
		Expect(mgr.GetCache().WaitForCacheSync(managerCtx)).To(BeTrue())

		repository := readyRepository("watch-parent")
		repository.Namespace = namespace.Name
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		repository.Status = readyRepositoryStatus(repository.Name)
		Expect(k8sClient.Status().Update(ctx, repository)).To(Succeed())
		source := parentVolumeClaim(repository, repository.Status.VolumeClaimName)
		Expect(controllerutil.SetControllerReference(repository, source, scheme)).To(Succeed())
		Expect(k8sClient.Create(ctx, source)).To(Succeed())
		source.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: source.Spec.Resources.Requests.DeepCopy()}
		Expect(k8sClient.Status().Update(ctx, source)).To(Succeed())
		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{Name: "watch-child", Namespace: namespace.Name},
			Spec:       repositoriesv1alpha1.WorktreeSpec{RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repository.Name}, Branch: "main"},
		}
		Expect(k8sClient.Create(ctx, worktree)).To(Succeed())
		key := client.ObjectKeyFromObject(worktree)
		Eventually(func() []ctrl.Request { return r.worktreesForClaim(ctx, source) }, time.Second*10).Should(Equal([]ctrl.Request{{NamespacedName: key}}))
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, worktree)).To(Succeed())
			ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Reason).To(Equal("Provisioning"))
		}, time.Second*10).Should(Succeed())

		// Provisioning has no timer requeue; only the single PVC watch can wake
		// the child here. No manual Reconcile call participates in this test.
		child := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, key, child)).To(Succeed())
		child.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, child)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, worktree)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionVolumeReady)).To(BeTrue())
		}, time.Second*10).Should(Succeed())
	})
})
