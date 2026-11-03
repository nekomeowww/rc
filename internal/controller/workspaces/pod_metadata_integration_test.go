//go:build integration

package workspaces

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var _ = Describe("Workspace runtime Pod metadata API", func() {
	It("rejects keys reserved for rc and accepts other labels, annotations, and priority", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pod-metadata-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-metadata", Namespace: namespace.Name},
			Spec: workspacesv1alpha1.WorkspaceSpec{
				PriorityClassName: "workspace-high",
				PodMetadata: &workspacesv1alpha1.WorkspacePodMetadata{
					Labels:      map[string]string{"example.com/tenant": testTenantA},
					Annotations: map[string]string{"karpenter.sh/do-not-disrupt": testTrueValue},
				},
			},
		}
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())

		for _, key := range []string{
			workspaceManagedByLabel,
			"rc.ayaka.io/repository",
		} {
			workspace.Spec.PodMetadata.Labels = map[string]string{key: testForgedValue}
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, workspace))).To(BeTrue(), key)
		}
		workspace.Spec.PodMetadata.Labels = nil
		workspace.Spec.PodMetadata.Annotations = map[string]string{workspaceTopologyAnnotation: testForgedValue}
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, workspace))).To(BeTrue())
		workspace.Spec.PodMetadata.Annotations = map[string]string{"notrc.ayaka.io/tenant": testTenantA}
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
		workspace.Spec.PriorityClassName = "Not_A_Name"
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, workspace))).To(BeTrue())
	})
})
