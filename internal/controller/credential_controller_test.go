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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

var _ = Describe("Credential Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		credential := &configsv1alpha1.Credential{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Credential")
			err := k8sClient.Get(ctx, typeNamespacedName, credential)
			if err != nil && errors.IsNotFound(err) {
				resource := &configsv1alpha1.Credential{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: configsv1alpha1.CredentialSpec{
						Type: configsv1alpha1.CredentialTypeSSHPrivateKey,
						SecretKeyRef: configsv1alpha1.SecretKeyReference{
							Name: "git-ssh",
							Key:  "ssh-privatekey",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &configsv1alpha1.Credential{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Credential")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &CredentialReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			persisted := &configsv1alpha1.Credential{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persisted)).To(Succeed())
			Expect(persisted.Spec.Type).To(Equal(configsv1alpha1.CredentialTypeSSHPrivateKey))
			Expect(persisted.Spec.SecretKeyRef).To(Equal(configsv1alpha1.SecretKeyReference{
				Name: "git-ssh",
				Key:  "ssh-privatekey",
			}))
		})
	})

	It("rejects an unsupported credential type", func() {
		credential := &configsv1alpha1.Credential{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unsupported-type",
				Namespace: "default",
			},
			Spec: configsv1alpha1.CredentialSpec{
				Type: "Password",
				SecretKeyRef: configsv1alpha1.SecretKeyReference{
					Name: "password",
					Key:  "password",
				},
			},
		}

		Expect(k8sClient.Create(ctx, credential)).NotTo(Succeed())
	})
})
