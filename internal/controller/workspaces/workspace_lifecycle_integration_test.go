//go:build integration

package workspaces

import (
	"context"
	"time"

	"github.com/nekomeowww/rc/internal/workspaceadmission"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var _ = Describe("Workspace lifecycle API", func() {
	It("keeps both clocks disabled by default and validates explicit durations", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lifecycle-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "default-retained", Namespace: namespace.Name}}
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		Expect(workspace.Spec.RetentionPolicy).To(Equal(workspacesv1alpha1.WorkspaceRetentionPolicyRetain))
		Expect(workspace.Spec.IdleTimeout).To(BeNil())
		Expect(workspace.Spec.DeleteAfterSuspended).To(BeNil())
		workspace.Spec.IdleTimeout = &metav1.Duration{Duration: time.Hour}
		workspace.Spec.DeleteAfterSuspended = &metav1.Duration{}
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
		Expect(workspace.Spec.DeleteAfterSuspended.Duration).To(BeZero())
		workspace.Spec.IdleTimeout.Duration = -time.Hour
		Expect(k8sClient.Update(ctx, workspace)).NotTo(Succeed())
		workspace.Spec.IdleTimeout.Duration = time.Hour
		workspace.Spec.DeleteAfterSuspended.Duration = -time.Hour
		Expect(k8sClient.Update(ctx, workspace)).NotTo(Succeed())
		workspace.Spec.DeleteAfterSuspended.Duration = 24 * time.Hour
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
	})
	It("serializes unchanged status admission with closure and rejects a reopened DELETE snapshot", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "admission-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, namespace)).To(Succeed()) })
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "admission", Namespace: namespace.Name}}
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "command", Namespace: namespace.Name, Finalizers: []string{executionFinalizer}}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: workspace.Name}, Command: []string{testTrueValue}}}
		Expect(k8sClient.Create(ctx, process)).To(Succeed())
		gate := workspaceadmission.Gate{Client: k8sClient}
		Expect(gate.Admit(ctx, workspace, process)).To(Succeed())
		// A successful no-op admission need not change resourceVersion. Closing
		// must still find the committed execution in its authoritative scan.
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(workspace), workspace)).To(Succeed())
		Expect(gate.Close(ctx, workspace)).To(Succeed())
		active, _, _, stateErr := workspaceProcessState(ctx, k8sClient, workspace)
		Expect(stateErr).NotTo(HaveOccurred())
		Expect(active).To(BeTrue())
		Expect(gate.Reopen(ctx, workspace)).To(Succeed())
		watchedClient, clientErr := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(clientErr).NotTo(HaveOccurred())
		intercepted := interceptor.NewClient(watchedClient, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, kubeClient client.Client, subResource string, object client.Object, opts ...client.SubResourceUpdateOption) error {
				current := new(workspacesv1alpha1.Workspace)
				Expect(kubeClient.Get(ctx, client.ObjectKeyFromObject(workspace), current)).To(Succeed())
				Expect(gate.Close(ctx, current)).To(Succeed())
				return kubeClient.SubResource(subResource).Update(ctx, object, opts...)
			},
		})
		// A no-op status write must still enforce the supplied resourceVersion.
		err := (workspaceadmission.Gate{Client: intercepted}).Admit(ctx, workspace, process)
		Expect(apierrors.IsConflict(err)).To(BeTrue())
		Expect(gate.Reopen(ctx, workspace)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(workspace), workspace)).To(Succeed())
		closedSnapshot := workspace.DeepCopy()
		Expect(gate.Close(ctx, closedSnapshot)).To(Succeed())
		Expect(gate.Reopen(ctx, workspace)).To(Succeed())
		err = k8sClient.Delete(ctx, closedSnapshot, client.Preconditions{UID: &closedSnapshot.UID, ResourceVersion: &closedSnapshot.ResourceVersion})
		Expect(apierrors.IsConflict(err)).To(BeTrue())
	})

})
