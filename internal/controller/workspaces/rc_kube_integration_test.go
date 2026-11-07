//go:build integration

package workspaces

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/rcplatform"
)

var _ = Describe("Injected rc-kube", func() {
	It("builds Linux runtime and editor Pods that the API server accepts", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "rc-kube-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })

		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "injected", Namespace: namespace.Name, UID: "injected-uid"},
			Spec: workspacesv1alpha1.WorkspaceSpec{
				Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")},
				Lifecycle: &workspacesv1alpha1.WorkspaceLifecycle{
					Initialize: []workspacesv1alpha1.WorkspaceLifecycleAction{
						{
							Script: "true",
						},
					},
					BeforeStop: []workspacesv1alpha1.WorkspaceLifecycleAction{
						{
							Script: "true",
						},
					},
				},
			},
		}
		reconciler := &WorkspaceReconciler{Client: k8sClient, APIReader: k8sClient, RunnerImage: testRunnerImage, RCKubeImage: testRCKubeImage}
		resolved, reason, _, err := reconciler.resolveWorkspaceBase(ctx, workspace)
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(BeEmpty())
		resolved, reason, _, err = reconciler.resolveWorkspaceDependencies(ctx, workspace, resolved)
		Expect(err).NotTo(HaveOccurred())
		Expect(reason).To(BeEmpty())
		resolved.homeClaimName = workspace.Name

		pod, err := workspaceRuntimePod(workspace, resolved)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, pod)).To(Succeed(), "the API server accepts the install container and its mounts")
		stored := new(corev1.Pod)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), stored)).To(Succeed())
		Expect(stored.Spec.InitContainers[0].Name).To(Equal(rcplatform.RCKubeInstallContainerName))
		Expect(stored.Spec.InitContainers[0].Image).To(Equal(testRCKubeImage))
		platform, err := rcplatform.FromPod(stored)
		Expect(err).NotTo(HaveOccurred())
		Expect(platform.ProcessTarget(namespace.Name, stored.Name, runtimeContainerName).Executable).To(Equal(rcplatform.RCKubeExecutable))

		environment := &workspacesv1alpha1.WorkspaceEnvironment{
			ObjectMeta: metav1.ObjectMeta{Name: "injected", Namespace: namespace.Name, UID: "environment-uid"},
			Spec:       workspacesv1alpha1.WorkspaceEnvironmentSpec{Image: testRunnerImage},
		}
		editor, err := environmentEditorPod(environment, "injected-draft-1", testRCKubeImage)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Create(ctx, editor)).To(Succeed(), "the API server accepts the editor Pod")
	})
})
