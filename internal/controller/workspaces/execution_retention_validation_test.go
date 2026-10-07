//go:build integration

package workspaces

import (
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const runtimeRunningPhase = "Running"

var _ = Describe("Execution retention API", func() {
	It("defaults only opted-in policies and validates positive limits", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "retention-api-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, namespace) })
		workspace := &workspacesv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "retention-api-workspace", Namespace: namespace.Name}}
		Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
		Expect(workspace.Spec.ExecutionRetention).To(BeNil())
		workspace.Spec.ExecutionRetention = &workspacesv1alpha1.ExecutionRetentionPolicy{}
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
		Expect(workspace.Spec.ExecutionRetention.MaxEntries).To(Equal(int32(500)))
		Expect(workspace.Spec.ExecutionRetention.TTLAfterFinished.Duration).To(Equal(7 * 24 * time.Hour))
		Expect(workspace.Spec.ExecutionRetention.TranscriptTTL.Duration).To(Equal(14 * 24 * time.Hour))
		workspace.Spec.ExecutionRetention.TranscriptTTL = &metav1.Duration{Duration: -time.Hour}
		Expect(k8sClient.Update(ctx, workspace)).NotTo(Succeed())
		workspace.Spec.ExecutionRetention.TranscriptTTL = nil
		workspace.Spec.ExecutionRetention.MaxEntries = -1
		Expect(k8sClient.Update(ctx, workspace)).NotTo(Succeed())
	})
	It("permits changing retain while preserving immutable execution identity and phase selectors", func() {
		process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{GenerateName: "retention-exec-", Namespace: testAPINamespace}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace, Name: "retention-api-target"}, Command: []string{testTrueValue}}}
		Expect(k8sClient.Create(ctx, process)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, process) })
		process.Spec.Retain = true
		Expect(k8sClient.Update(ctx, process)).To(Succeed())
		process.Status.Phase = workspacesv1alpha1.WorkspaceExecPhaseRunning
		Expect(k8sClient.Status().Update(ctx, process)).To(Succeed())
		list := new(workspacesv1alpha1.WorkspaceExecList)
		Expect(k8sClient.List(ctx, list, client.InNamespace(testAPINamespace), client.MatchingFields{"status.phase": runtimeRunningPhase, "spec.targetRef.name": "retention-api-target"})).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		process.Spec.Command = []string{"changed"}
		Expect(k8sClient.Update(ctx, process)).NotTo(Succeed())
	})
})
