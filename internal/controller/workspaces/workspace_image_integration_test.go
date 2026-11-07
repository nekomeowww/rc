//go:build integration

package workspaces

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var _ = Describe("Workspace image upgrades", func() {
	It("starts a suspended Workspace on a changed image and keeps a running Pod", func() {
		Expect(repositoriesv1alpha1.AddToScheme(k8sClient.Scheme())).To(Succeed())
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "image-upgrade-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })

		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "upgrade", Namespace: namespace.Name},
			Spec: workspacesv1alpha1.WorkspaceSpec{
				Image:   testRuntimeImage,
				Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")},
			},
		}
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		key := client.ObjectKeyFromObject(workspace)
		reconciler := &WorkspaceReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: testRunnerImage}
		reconcileWorkspace := func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, workspace)).To(Succeed())
		}
		runtimePod := func() *corev1.Pod {
			pod := new(corev1.Pod)
			Expect(k8sClient.Get(ctx, key, pod)).To(Succeed())
			return pod
		}
		runtimeImage := func(pod *corev1.Pod) string {
			for _, container := range pod.Spec.Containers {
				if container.Name == runtimeContainerName {
					return container.Image
				}
			}
			return ""
		}
		setDesiredState := func(state workspacesv1alpha1.WorkspaceDesiredState) {
			Expect(k8sClient.Get(ctx, key, workspace)).To(Succeed())
			workspace.Spec.DesiredState = state
			Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
		}

		// Bind the home volume: envtest has no provisioner.
		reconcileWorkspace()
		reconcileWorkspace()
		home := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workspace.Status.HomeVolumeClaimName, Namespace: namespace.Name}, home)).To(Succeed())
		home.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, home)).To(Succeed())

		reconcileWorkspace()
		first := runtimePod()
		Expect(runtimeImage(first)).To(Equal(testRuntimeImage))
		Expect(workspace.Status.RuntimeImage).To(Equal(testRuntimeImage))

		By("changing spec.image while the runtime Pod runs")
		workspace.Spec.Image = testUpgradedImage
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
		reconcileWorkspace()
		running := runtimePod()
		Expect(running.UID).To(Equal(first.UID), "an image change does not replace the running Pod")
		Expect(runtimeImage(running)).To(Equal(testRuntimeImage))
		Expect(workspace.Status.RuntimeImage).To(Equal(testRuntimeImage))
		outdated := meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionOutdated)
		Expect(outdated).NotTo(BeNil())
		Expect(outdated.Status).To(Equal(metav1.ConditionTrue))
		Expect(outdated.Reason).To(Equal(workspacesv1alpha1.WorkspaceReasonImageChangePending))

		By("suspending the Workspace")
		setDesiredState(workspacesv1alpha1.WorkspaceDesiredStateSuspended)
		Eventually(func() bool {
			reconcileWorkspace()
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, new(corev1.Pod)))
		}).Should(BeTrue())
		reconcileWorkspace()
		Expect(workspace.Status.RuntimeImage).To(Equal(testRuntimeImage))

		By("starting the Workspace again")
		setDesiredState(workspacesv1alpha1.WorkspaceDesiredStateRunning)
		reconcileWorkspace()
		started := runtimePod()
		Expect(runtimeImage(started)).To(Equal(testUpgradedImage))
		Expect(workspace.Status.RuntimeImage).To(Equal(testUpgradedImage))
		outdated = meta.FindStatusCondition(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionOutdated)
		Expect(outdated).NotTo(BeNil())
		Expect(outdated.Status).To(Equal(metav1.ConditionFalse))

		By("reconciling the new Pod again")
		reconcileWorkspace()
		Expect(runtimePod().UID).To(Equal(started.UID), "the recorded image matches the topology of the new Pod")
	})
})
