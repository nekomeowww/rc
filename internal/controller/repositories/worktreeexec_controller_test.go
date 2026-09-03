/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package repositories

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreebootstrap"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

const (
	testWorktreeExecName = "feature-exec"
	testWorktreeExecTree = "feature"
	testWorktreeExecGit  = "git"
	testWorktreeExecArg  = "--short"
	testWorktreeUID      = "worktree-uid"
	testExecUID          = "exec-uid"
	testWorkspaceUID     = "workspace-uid"
	testRunnerImage      = "runner:test"
	testReadyReason      = "WorktreeReady"
)

var _ = Describe("WorktreeExec Job", func() {
	It("executes the exact argv in the native Worktree child volume", func() {
		exec := &repositoriesv1alpha1.WorktreeExec{
			ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecName, Namespace: testNamespace},
			Spec: repositoriesv1alpha1.WorktreeExecSpec{
				WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: testWorktreeExecTree},
				Command:     []string{testWorktreeExecGit, testStatusArgument, testWorktreeExecArg},
			},
		}
		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecTree, Namespace: testNamespace},
			Status: repositoriesv1alpha1.WorktreeStatus{
				VolumeClaimName: "feature-pvc", WorktreePath: "/repository/worktree/feature",
			},
		}

		job := worktreeExecJob(exec, worktree, testRunnerImage)
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Command).To(Equal([]string{testWorktreeExecGit}))
		Expect(container.Args).To(Equal([]string{testStatusArgument, testWorktreeExecArg}))
		Expect(container.WorkingDir).To(Equal(worktreebootstrap.NativeWorktreeMountPath(worktree.Name)))
		Expect(container.VolumeMounts[0].MountPath).To(Equal(worktreebootstrap.VolumeRootMountPath(worktree.Name)))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("feature-pvc"))
	})

	It("uses the volume root for an in-place generated Worktree", func() {
		exec := &repositoriesv1alpha1.WorktreeExec{
			Spec: repositoriesv1alpha1.WorktreeExecSpec{Command: []string{"pwd"}},
		}
		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{Name: "generated"},
			Status:     repositoriesv1alpha1.WorktreeStatus{WorktreePath: workerMountPath},
		}

		job := worktreeExecJob(exec, worktree, testRunnerImage)
		Expect(job.Spec.Template.Spec.Containers[0].WorkingDir).To(Equal(worktreebootstrap.VolumeRootMountPath(worktree.Name)))
	})

	It("waits when the Workspace-compatible write Lease has another holder", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecTree, Namespace: testNamespace, UID: testWorktreeUID}}
		exec := &repositoriesv1alpha1.WorktreeExec{ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecName, Namespace: testNamespace, UID: testExecUID}}
		workspaceHolder := testWorkspaceUID
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: testNamespace},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &workspaceHolder},
		}
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lease).Build()
		reconciler := &WorktreeExecReconciler{Client: client, Scheme: scheme}

		acquired, err := reconciler.acquireClaim(context.Background(), exec, worktree)

		Expect(err).NotTo(HaveOccurred())
		Expect(acquired).To(BeFalse())
	})

	It("releases its write Lease after execution", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		exec := &repositoriesv1alpha1.WorktreeExec{ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecName, Namespace: testNamespace, UID: testExecUID}}
		holder := string(exec.UID)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "rc-worktree-test", Namespace: testNamespace, Labels: map[string]string{worktreeclaim.HolderLabel: exec.Name}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		}
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lease).Build()
		reconciler := &WorktreeExecReconciler{Client: client, Scheme: scheme}

		Expect(reconciler.releaseClaim(context.Background(), exec)).To(Succeed())
		persisted := new(coordinationv1.Lease)
		err := client.Get(context.Background(), types.NamespacedName{Name: lease.Name, Namespace: lease.Namespace}, persisted)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("marks a recorded missing Job lost instead of executing the command again", func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		worktree := readyWorktreeForExec()
		exec := worktreeExecWithRecordedJob()
		client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&repositoriesv1alpha1.WorktreeExec{}).WithObjects(worktree, exec).Build()
		reconciler := &WorktreeExecReconciler{Client: client, Scheme: scheme, RunnerImage: testRunnerImage}

		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: clientObjectKey(exec)})

		Expect(err).NotTo(HaveOccurred())
		job := new(batchv1.Job)
		Expect(apierrors.IsNotFound(client.Get(context.Background(), clientObjectKey(exec), job))).To(BeTrue())
		persisted := new(repositoriesv1alpha1.WorktreeExec)
		Expect(client.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
		condition := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("JobLost"))
	})

	It("restores a missing write Lease while a Job is running", func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		worktree := readyWorktreeForExec()
		exec := worktreeExecWithRecordedJob()
		job := runningWorktreeExecJob(scheme, exec)
		client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&repositoriesv1alpha1.WorktreeExec{}, &batchv1.Job{}).WithObjects(worktree, exec, job).Build()
		reconciler := &WorktreeExecReconciler{Client: client, Scheme: scheme, RunnerImage: testRunnerImage}

		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: clientObjectKey(exec)})

		Expect(err).NotTo(HaveOccurred())
		lease := new(coordinationv1.Lease)
		Expect(client.Get(context.Background(), types.NamespacedName{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace}, lease)).To(Succeed())
		Expect(lease.Spec.HolderIdentity).To(HaveValue(Equal(string(exec.UID))))
	})

	It("stops a running Job when another writer owns its Lease", func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		worktree := readyWorktreeForExec()
		exec := worktreeExecWithRecordedJob()
		job := runningWorktreeExecJob(scheme, exec)
		foreignHolder := testWorkspaceUID
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &foreignHolder},
		}
		client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&repositoriesv1alpha1.WorktreeExec{}, &batchv1.Job{}).WithObjects(worktree, exec, job, lease).Build()
		reconciler := &WorktreeExecReconciler{Client: client, Scheme: scheme, RunnerImage: testRunnerImage}

		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: clientObjectKey(exec)})

		Expect(err).NotTo(HaveOccurred())
		persistedJob := new(batchv1.Job)
		Expect(apierrors.IsNotFound(client.Get(context.Background(), clientObjectKey(exec), persistedJob))).To(BeTrue())
		persisted := new(repositoriesv1alpha1.WorktreeExec)
		Expect(client.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
		condition := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition.Reason).To(Equal("StoppingAfterWriteLeaseLost"))

		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: clientObjectKey(exec)})

		Expect(err).NotTo(HaveOccurred())
		Expect(client.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
		condition = meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("WriteLeaseLost"))
	})

	It("records the execution before an ambiguous Job creation failure", func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		worktree := readyWorktreeForExec()
		exec := worktreeExecWithRecordedJob()
		exec.Status = repositoriesv1alpha1.WorktreeExecStatus{}
		baseClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&repositoriesv1alpha1.WorktreeExec{}).WithObjects(worktree, exec).Build()
		createErr := errors.New("injected ambiguous Job creation error")
		observingClient := &failingJobCreateClient{Client: baseClient, createErr: createErr, beforeFailure: func() {
			persisted := new(repositoriesv1alpha1.WorktreeExec)
			Expect(baseClient.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
			Expect(persisted.Status.JobName).To(Equal(exec.Name))
			condition := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Reason).To(Equal("JobScheduled"))
		}}
		reconciler := &WorktreeExecReconciler{Client: observingClient, Scheme: scheme, RunnerImage: testRunnerImage}

		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: clientObjectKey(exec)})

		Expect(err).To(MatchError(ContainSubstring(createErr.Error())))
		lease := new(coordinationv1.Lease)
		Expect(baseClient.Get(context.Background(), types.NamespacedName{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace}, lease)).To(Succeed())
		reconciler.Client = baseClient
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: clientObjectKey(exec)})
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(baseClient.Get(context.Background(), clientObjectKey(lease), lease))).To(BeTrue())
		persisted := new(repositoriesv1alpha1.WorktreeExec)
		Expect(baseClient.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
		condition := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("JobLost"))
	})

	It("maps a foreign Lease change to waiting WorktreeExec resources", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		worktree := readyWorktreeForExec()
		exec := worktreeExecWithRecordedJob()
		exec.Status = repositoriesv1alpha1.WorktreeExecStatus{}
		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, exec).Build()
		reconciler := &WorktreeExecReconciler{Client: kubeClient}
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace,
		}}

		requests := reconciler.execsForLease(context.Background(), lease)

		Expect(requests).To(ConsistOf(reconcile.Request{NamespacedName: clientObjectKey(exec)}))
	})

	It("keeps its Lease until Pods from a lost Job are gone", func() {
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		exec := worktreeExecWithRecordedJob()
		holder := string(exec.UID)
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: "lost-job-lease", Namespace: exec.Namespace, Labels: map[string]string{worktreeclaim.HolderLabel: exec.Name}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: "orphaned-command-pod", Namespace: exec.Namespace, Labels: map[string]string{"job-name": exec.Status.JobName},
		}}
		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&repositoriesv1alpha1.WorktreeExec{}).WithObjects(exec, lease, pod).Build()
		reconciler := &WorktreeExecReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}
		request := reconcile.Request{NamespacedName: clientObjectKey(exec)}

		Expect(reconciler.execsForJobPod(context.Background(), pod)).To(ConsistOf(request))
		_, err := reconciler.Reconcile(context.Background(), request)

		Expect(err).NotTo(HaveOccurred())
		persistedLease := new(coordinationv1.Lease)
		Expect(kubeClient.Get(context.Background(), clientObjectKey(lease), persistedLease)).To(Succeed())
		persisted := new(repositoriesv1alpha1.WorktreeExec)
		Expect(kubeClient.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
		condition := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionUnknown))
		Expect(condition.Reason).To(Equal("StoppingAfterJobLost"))

		_, err = reconciler.Reconcile(context.Background(), request)

		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(kubeClient.Get(context.Background(), clientObjectKey(lease), persistedLease))).To(BeTrue())
		Expect(kubeClient.Get(context.Background(), clientObjectKey(exec), persisted)).To(Succeed())
		condition = meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeExecConditionSucceeded)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("JobLost"))
	})
})

func readyWorktreeForExec() *repositoriesv1alpha1.Worktree {
	return &repositoriesv1alpha1.Worktree{
		ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecTree, Namespace: testNamespace, UID: testWorktreeUID, Generation: 1},
		Status: repositoriesv1alpha1.WorktreeStatus{ObservedGeneration: 1, VolumeClaimName: "feature-pvc", Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: testReadyReason,
		}}},
	}
}

func worktreeExecWithRecordedJob() *repositoriesv1alpha1.WorktreeExec {
	return &repositoriesv1alpha1.WorktreeExec{
		ObjectMeta: metav1.ObjectMeta{Name: testWorktreeExecName, Namespace: testNamespace, UID: testExecUID, Generation: 1},
		Spec: repositoriesv1alpha1.WorktreeExecSpec{
			WorktreeRef: repositoriesv1alpha1.WorktreeReference{Name: testWorktreeExecTree}, Command: []string{"true"},
		},
		Status: repositoriesv1alpha1.WorktreeExecStatus{JobName: testWorktreeExecName, Conditions: []metav1.Condition{{
			Type: repositoriesv1alpha1.WorktreeExecConditionSucceeded, Status: metav1.ConditionUnknown, ObservedGeneration: 1, Reason: "JobCreated",
		}}},
	}
}

func runningWorktreeExecJob(scheme *runtime.Scheme, exec *repositoriesv1alpha1.WorktreeExec) *batchv1.Job {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: exec.Name, Namespace: exec.Namespace}}
	Expect(controllerutil.SetControllerReference(exec, job, scheme)).To(Succeed())
	return job
}

func clientObjectKey(object metav1.Object) types.NamespacedName {
	return types.NamespacedName{Name: object.GetName(), Namespace: object.GetNamespace()}
}
