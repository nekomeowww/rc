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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

var _ = Describe("Repository Controller", func() {
	It("creates and reports the parent PersistentVolumeClaim", func() {
		const repositoryName = "repository-parent-volume"
		ctx := context.Background()
		key := types.NamespacedName{Name: repositoryName, Namespace: testNamespace}
		repository := &repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: repositoryName, Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: testRemoteURL},
				Storage: repositoriesv1alpha1.RepositoryStorageSpec{
					StorageClassName: testStorageClassName,
					Size:             resource.MustParse("1Gi"),
				},
			},
		}
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repository)).To(Succeed()) })

		reconciler := &RepositoryReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), WorkerImage: "repository-worker:test"}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		claim := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, key, claim)).To(Succeed())
		Expect(claim.Spec.StorageClassName).To(HaveValue(Equal(testStorageClassName)))
		Expect(claim.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))
		Expect(claim.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("1Gi")))
		Expect(metav1.IsControlledBy(claim, repository)).To(BeTrue())

		persisted := new(repositoriesv1alpha1.Repository)
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		ready := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(persisted.Status.VolumeClaimName).To(Equal(repositoryName))

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		bootstrapJob := new(batchv1.Job)
		Expect(k8sClient.Get(ctx, keyWithName(repositoryBootstrapJobName(repository)), bootstrapJob)).To(Succeed())
		Expect(bootstrapJob.Spec.Template.Spec.Containers[0].Image).To(Equal("repository-worker:test"))
		Expect(bootstrapJob.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(repositoryName))
		Expect(bootstrapJob.Spec.Template.Spec.Containers[0].Args).To(ContainElement(testRemoteURL))

		claim.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
		completedAt := metav1.Now()
		bootstrapJob.Status.StartTime = &completedAt
		bootstrapJob.Status.CompletionTime = &completedAt
		bootstrapJob.Status.Succeeded = 1
		bootstrapJob.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: completedAt},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completedAt},
		}
		Expect(k8sClient.Status().Update(ctx, bootstrapJob)).To(Succeed())
		persistedJob := new(batchv1.Job)
		Expect(k8sClient.Get(ctx, keyWithName(repositoryBootstrapJobName(repository)), persistedJob)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		ready = meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal("RepositoryReady"))
		Expect(persisted.Status.LastUpdatedAt).NotTo(BeNil())
		Expect(persistedJob.Status.CompletionTime).NotTo(BeNil())
		Expect(persisted.Status.LastUpdatedAt.Time).To(Equal(persistedJob.Status.CompletionTime.Time))

		// A controller upgrade should backfill the timestamp from the existing
		// successful Job without re-running the Git bootstrap.
		persisted.Status.LastUpdatedAt = nil
		Expect(k8sClient.Status().Update(ctx, persisted)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		Expect(persisted.Status.LastUpdatedAt).NotTo(BeNil())
		Expect(persisted.Status.LastUpdatedAt.Time).To(Equal(persistedJob.Status.CompletionTime.Time))
	})

	It("rejects a non-positive parent volume size", func() {
		repository := &repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-zero-size", Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: testRemoteURL},
				Storage: repositoriesv1alpha1.RepositoryStorageSpec{
					StorageClassName: testStorageClassName,
					Size:             resource.MustParse("0"),
				},
			},
		}
		Expect(k8sClient.Create(context.Background(), repository)).NotTo(Succeed())
	})

	It("mounts SSH credential references into the bootstrap Job", func() {
		const credentialName = "git-ssh"
		repository := &repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-bootstrap-ssh", Namespace: testNamespace, Generation: 1},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Remote: repositoriesv1alpha1.RepositoryRemoteSpec{
					URL: "ssh://git@example.test/repository.git",
					CredentialRef: &repositoriesv1alpha1.RepositoryCredentialReference{
						Name: credentialName,
					},
				},
				Storage: repositoriesv1alpha1.RepositoryStorageSpec{
					StorageClassName: testStorageClassName,
					Size:             resource.MustParse("1Gi"),
				},
			},
		}
		credential := &configsv1alpha1.Credential{
			Spec: configsv1alpha1.CredentialSpec{
				Type: configsv1alpha1.CredentialTypeSSHPrivateKey,
				SSHPrivateKey: &configsv1alpha1.SSHPrivateKeyCredential{
					PrivateKeyRef: configsv1alpha1.SecretKeyReference{Name: credentialName, Key: "private-key"},
					KnownHostsRef: configsv1alpha1.SecretKeyReference{Name: credentialName, Key: "known-hosts"},
				},
			},
		}

		job := repositoryBootstrapJob(repository, "repository-worker:test", credential)
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Env).To(ContainElement(corev1.EnvVar{
			Name:  "GIT_SSH_COMMAND",
			Value: "ssh -i /run/rc/credentials/ssh-private-key -o UserKnownHostsFile=/run/rc/credentials/ssh-known-hosts -o IdentitiesOnly=yes",
		}))
		Expect(job.Spec.Template.Spec.Volumes[1].Secret.SecretName).To(Equal(credentialName))
		Expect(job.Spec.Template.Spec.Volumes[2].Secret.SecretName).To(Equal(credentialName))
		Expect(container.Args).To(ContainElement("ssh"))
	})

	It("accepts a full Git ref or commit and rejects a shorthand ref", func() {
		ctx := context.Background()
		validRefs := []string{
			"refs/heads/main",
			"refs/tags/v1.0.0",
			"refs/pull/42/head",
			"0123456789abcdef0123456789abcdef01234567",
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}
		for index, ref := range validRefs {
			repository := &repositoriesv1alpha1.Repository{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("repository-valid-ref-%d", index), Namespace: testNamespace},
				Spec: repositoriesv1alpha1.RepositorySpec{
					Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: testRemoteURL},
					Ref:    ref,
					Storage: repositoriesv1alpha1.RepositoryStorageSpec{
						StorageClassName: testStorageClassName,
						Size:             resource.MustParse("1Gi"),
					},
				},
			}
			Expect(k8sClient.Create(ctx, repository)).To(Succeed())
			Expect(k8sClient.Delete(ctx, repository)).To(Succeed())
		}

		repository := &repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-shorthand-ref", Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Remote: repositoriesv1alpha1.RepositoryRemoteSpec{URL: testRemoteURL},
				Ref:    "main",
				Storage: repositoriesv1alpha1.RepositoryStorageSpec{
					StorageClassName: testStorageClassName,
					Size:             resource.MustParse("1Gi"),
				},
			},
		}
		Expect(k8sClient.Create(ctx, repository)).NotTo(Succeed())
	})

	It("requires an explicit opt-in for an unencrypted HTTP remote", func() {
		ctx := context.Background()
		insecureRemote := repositoriesv1alpha1.RepositoryRemoteSpec{URL: "http://git.example.test/repository.git"}
		repository := &repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-insecure-http", Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Remote: insecureRemote,
				Storage: repositoriesv1alpha1.RepositoryStorageSpec{
					StorageClassName: testStorageClassName,
					Size:             resource.MustParse("1Gi"),
				},
			},
		}
		Expect(k8sClient.Create(ctx, repository)).NotTo(Succeed())

		repository.Name = "repository-allowed-http"
		repository.Spec.Remote.AllowInsecureHTTP = true
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		Expect(k8sClient.Delete(ctx, repository)).To(Succeed())

		repository.Name = "repository-uppercase-http"
		repository.Spec.Remote.URL = "HTTP://git.example.test/repository.git"
		repository.Spec.Remote.AllowInsecureHTTP = false
		Expect(k8sClient.Create(ctx, repository)).NotTo(Succeed())
	})

	It("rejects a Repository without a remote URL", func() {
		repository := &repositoriesv1alpha1.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "repository-missing-remote-url", Namespace: testNamespace},
			Spec: repositoriesv1alpha1.RepositorySpec{
				Storage: repositoriesv1alpha1.RepositoryStorageSpec{
					StorageClassName: testStorageClassName,
					Size:             resource.MustParse("1Gi"),
				},
			},
		}
		Expect(k8sClient.Create(context.Background(), repository)).NotTo(Succeed())
	})
})

func keyWithName(name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: testNamespace}
}
