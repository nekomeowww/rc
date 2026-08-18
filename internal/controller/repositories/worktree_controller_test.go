package repositories

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

var _ = Describe("Worktree Controller", func() {
	It("clones the Repository PVC and creates a native Git worktree Job", func() {
		ctx := context.Background()
		const (
			repositoryName = "worktree-parent"
			worktreeName   = "worktree-child"
			workerImage    = "repository-worker:test"
		)
		repository := readyRepository(repositoryName)
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		repository.Status = readyRepositoryStatus(repositoryName)
		Expect(k8sClient.Status().Update(ctx, repository)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repository)).To(Succeed()) })

		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1Object(worktreeName),
			Spec: repositoriesv1alpha1.WorktreeSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repositoryName},
				Branch:        "feature/worktree",
			},
		}
		Expect(k8sClient.Create(ctx, worktree)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, worktree)).To(Succeed()) })

		reconciler := &WorktreeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), WorkerImage: workerImage}
		key := types.NamespacedName{Name: worktreeName, Namespace: testNamespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		claim := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, key, claim)).To(Succeed())
		Expect(claim.Spec.DataSource.Kind).To(Equal("PersistentVolumeClaim"))
		Expect(claim.Spec.DataSource.Name).To(Equal(repositoryName))
		Expect(claim.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}))

		claim.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())

		workloadPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "worktree-workload", Namespace: testNamespace},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "workload",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
				}},
				Volumes: []corev1.Volume{{
					Name: "worktree",
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: worktreeName,
					}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, workloadPod)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, workloadPod)).To(Succeed()) })

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		persistedPod := new(corev1.Pod)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workloadPod.Name, Namespace: testNamespace}, persistedPod)).To(Succeed())
		Expect(persistedPod.Labels[worktreeUIDLabel]).To(Equal(string(worktree.UID)))

		job := new(batchv1.Job)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: worktreeBootstrapJobName(worktree), Namespace: testNamespace}, job)).To(Succeed())
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(workerImage))
		Expect(container.Args).To(ContainElement("worktree"))
		Expect(container.Args).To(ContainElement("add"))
		Expect(container.Args).To(ContainElement("-b"))
		Expect(container.Args).To(ContainElement("feature/worktree"))
		Expect(container.Args).To(ContainElement(worktreePath(worktree)))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(worktreeName))
		Expect(job.Spec.Template.Labels[worktreeUIDLabel]).To(Equal(string(worktree.UID)))
		Expect(metav1.IsControlledBy(job, worktree)).To(BeTrue())

		completedAt := metav1.Now()
		job.Status.StartTime = &completedAt
		job.Status.CompletionTime = &completedAt
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: completedAt},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completedAt},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		persisted := new(repositoriesv1alpha1.Worktree)
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		ready := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(persisted.Status.VolumeClaimName).To(Equal(worktreeName))
		Expect(persisted.Status.WorktreePath).To(Equal(worktreePath(worktree)))
	})
})

func metav1Object(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace}
}
