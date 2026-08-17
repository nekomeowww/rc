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

var _ = Describe("AgentCredential Controller", func() {
	const agentSecretKey = "auth.json"

	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = testNamespace
			agentSecretName   = "agent-auth"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		agentcredential := &configsv1alpha1.AgentCredential{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind AgentCredential")
			err := k8sClient.Get(ctx, typeNamespacedName, agentcredential)
			if err != nil && errors.IsNotFound(err) {
				resource := &configsv1alpha1.AgentCredential{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: configsv1alpha1.AgentCredentialSpec{
						Agent: configsv1alpha1.AgentTypeCodex,
						SecretKeyRef: configsv1alpha1.SecretKeyReference{
							Name: agentSecretName,
							Key:  agentSecretKey,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &configsv1alpha1.AgentCredential{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance AgentCredential")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &AgentCredentialReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			persisted := &configsv1alpha1.AgentCredential{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, persisted)).To(Succeed())
			Expect(persisted.Spec.Agent).To(Equal(configsv1alpha1.AgentTypeCodex))
			Expect(persisted.Spec.SecretKeyRef).To(Equal(configsv1alpha1.SecretKeyReference{
				Name: agentSecretName,
				Key:  agentSecretKey,
			}))
		})
	})

	It("rejects an unsupported agent type", func() {
		agentCredential := &configsv1alpha1.AgentCredential{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unsupported-agent",
				Namespace: testNamespace,
			},
			Spec: configsv1alpha1.AgentCredentialSpec{
				Agent: "other",
				SecretKeyRef: configsv1alpha1.SecretKeyReference{
					Name: "other-auth",
					Key:  agentSecretKey,
				},
			},
		}

		Expect(k8sClient.Create(ctx, agentCredential)).NotTo(Succeed())
	})
})
