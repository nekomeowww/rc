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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

var _ = Describe("RepositoryExec Controller", func() {
	It("passes argv to a serialized Job and reports success", func() {
		const (
			execName       = "repository-exec-command"
			repositoryName = "repository-for-exec"
			runnerImage    = "ghcr.io/example/rc/runner:test"
		)
		ctx := context.Background()
		repository := readyRepository(repositoryName)
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		repository.Status = readyRepositoryStatus(repositoryName)
		Expect(k8sClient.Status().Update(ctx, repository)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repository)).To(Succeed()) })

		exec := &repositoriesv1alpha1.RepositoryExec{
			ObjectMeta: metav1.ObjectMeta{Name: execName, Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositoryExecSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repositoryName},
				Command:       []string{testGitExecutable, testStatusArgument, "--short"},
			},
		}
		Expect(k8sClient.Create(ctx, exec)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, exec)).To(Succeed()) })

		reconciler := &RepositoryExecReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: runnerImage}
		execKey := types.NamespacedName{Name: execName, Namespace: testNamespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: execKey})
		Expect(err).NotTo(HaveOccurred())

		job := new(batchv1.Job)
		Expect(k8sClient.Get(ctx, execKey, job)).To(Succeed())
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(runnerImage))
		Expect(container.Command).To(Equal([]string{testGitExecutable}))
		Expect(container.Args).To(Equal([]string{testStatusArgument, "--short"}))
		Expect(container.WorkingDir).To(Equal(workerMountPath))
		Expect(job.Spec.TTLSecondsAfterFinished).To(HaveValue(Equal(int32(259200))))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(repositoryName))
		Expect(metav1.IsControlledBy(job, exec)).To(BeTrue())

		completedAt := metav1.Now()
		job.Status.StartTime = &completedAt
		job.Status.CompletionTime = &completedAt
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{
				Type:               batchv1.JobSuccessCriteriaMet,
				Status:             "True",
				Reason:             "CompletionsReached",
				LastProbeTime:      completedAt,
				LastTransitionTime: completedAt,
			},
			{
				Type:               batchv1.JobComplete,
				Status:             "True",
				Reason:             "Completed",
				LastProbeTime:      completedAt,
				LastTransitionTime: completedAt,
			},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: execKey})
		Expect(err).NotTo(HaveOccurred())

		persisted := new(repositoriesv1alpha1.RepositoryExec)
		Expect(k8sClient.Get(ctx, execKey, persisted)).To(Succeed())
		succeeded := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.RepositoryExecConditionSucceeded)
		Expect(succeeded).NotTo(BeNil())
		Expect(succeeded.Status).To(Equal(metav1.ConditionTrue))
		Expect(persisted.Status.JobName).To(Equal(execName))
	})

	It("rejects an empty command", func() {
		exec := &repositoriesv1alpha1.RepositoryExec{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-exec-empty", Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositoryExecSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: workerVolumeName},
			},
		}
		Expect(k8sClient.Create(context.Background(), exec)).NotTo(Succeed())
	})

	It("rejects changes to an accepted exec request", func() {
		ctx := context.Background()
		exec := &repositoriesv1alpha1.RepositoryExec{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-exec-immutable", Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositoryExecSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "repository"},
				Command:       []string{testGitExecutable, testStatusArgument},
			},
		}
		Expect(k8sClient.Create(ctx, exec)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, exec)).To(Succeed()) })

		exec.Spec.Command = []string{testGitExecutable, "reset", "--hard"}
		Expect(k8sClient.Update(ctx, exec)).NotTo(Succeed())
	})
})

func readyRepository(name string) *repositoriesv1alpha1.Repository {
	return &repositoriesv1alpha1.Repository{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: repositoriesv1alpha1.RepositorySpec{
			Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: testRemoteURL},
			Storage: repositoriesv1alpha1.RepositoryStorageSpec{
				StorageClassName: testStorageClassName,
				Size:             resource.MustParse("1Gi"),
			},
		},
	}
}

func readyRepositoryStatus(name string) repositoriesv1alpha1.RepositoryStatus {
	return repositoriesv1alpha1.RepositoryStatus{
		VolumeClaimName: name,
		Conditions: []metav1.Condition{{
			Type:               repositoriesv1alpha1.RepositoryConditionStorageReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
			Reason:             "VolumeReady",
			Message:            "Parent volume is bound",
		}},
	}
}
