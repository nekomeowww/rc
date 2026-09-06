package workspaces

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var _ = Describe("Runtime operating system API", func() {
	It("defaults Linux, accepts Windows, and prevents changing a persisted OS", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "runtime-os-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "desktop", Namespace: namespace.Name},
			Spec:       workspacesv1alpha1.WorkspaceSpec{OS: corev1.Windows, Storage: &workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")}},
		}
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		workspace.Spec.OS = corev1.Linux
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, workspace))).To(BeTrue())
		environment := &workspacesv1alpha1.WorkspaceEnvironment{
			ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace.Name},
			Spec:       workspacesv1alpha1.WorkspaceEnvironmentSpec{Image: "example/linux", Storage: workspacesv1alpha1.PersistentStorageSpec{Size: resource.MustParse("1Gi")}},
		}
		Expect(k8sClient.Create(ctx, environment)).To(Succeed())
		Expect(environment.Spec.OS).To(Equal(corev1.Linux))
		environment.Spec.OS = corev1.Windows
		Expect(apierrors.IsInvalid(k8sClient.Update(ctx, environment))).To(BeTrue())

		credential := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "configs.rc.ayaka.io/v1alpha1", "kind": "Credential",
			"metadata": map[string]any{"name": "windows-file", "namespace": namespace.Name},
			"spec": map[string]any{"type": "Process", "process": map[string]any{"files": []any{
				map[string]any{"mountPath": `C:\home\agent\.tool\auth.json`, "dataRef": map[string]any{"name": "fixture", "key": "auth"}},
			}}},
		}}
		Expect(k8sClient.Create(ctx, credential)).To(Succeed(), "Windows credential paths pass the generated CRD schema")
	})
})
