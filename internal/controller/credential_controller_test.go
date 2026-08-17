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

const (
	credentialTestHTTPSecretName   = "git-http"
	credentialTestSSHPrivateKeyKey = "ssh-privatekey"
	credentialTestSSHSecretName    = "git-ssh"
	credentialTestTokenKey         = "token"
)

var _ = Describe("Credential Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = testNamespace
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
						SSHPrivateKey: &configsv1alpha1.SSHPrivateKeyCredential{
							PrivateKeyRef: configsv1alpha1.SecretKeyReference{
								Name: credentialTestSSHSecretName,
								Key:  credentialTestSSHPrivateKeyKey,
							},
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
			Expect(persisted.Spec.SSHPrivateKey).NotTo(BeNil())
			Expect(persisted.Spec.SSHPrivateKey.PrivateKeyRef).To(Equal(configsv1alpha1.SecretKeyReference{
				Name: credentialTestSSHSecretName,
				Key:  credentialTestSSHPrivateKeyKey,
			}))
		})
	})

	It("accepts each supported credential type with its matching payload", func() {
		credentials := []*configsv1alpha1.Credential{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "http-basic", Namespace: testNamespace},
				Spec: configsv1alpha1.CredentialSpec{
					Type: configsv1alpha1.CredentialTypeHTTPBasicAuth,
					HTTPBasicAuth: &configsv1alpha1.HTTPBasicAuthCredential{
						Username: "git",
						PasswordRef: configsv1alpha1.SecretKeyReference{
							Name: credentialTestHTTPSecretName,
							Key:  credentialTestTokenKey,
						},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "http-bearer", Namespace: testNamespace},
				Spec: configsv1alpha1.CredentialSpec{
					Type: configsv1alpha1.CredentialTypeHTTPBearerToken,
					HTTPBearerToken: &configsv1alpha1.HTTPBearerTokenCredential{
						TokenRef: configsv1alpha1.SecretKeyReference{
							Name: credentialTestHTTPSecretName,
							Key:  credentialTestTokenKey,
						},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "http-headers", Namespace: testNamespace},
				Spec: configsv1alpha1.CredentialSpec{
					Type: configsv1alpha1.CredentialTypeHTTPHeaders,
					HTTPHeaders: &configsv1alpha1.HTTPHeadersCredential{
						Headers: []configsv1alpha1.HTTPHeader{
							{
								Name: "X-Internal-Token",
								ValueRef: configsv1alpha1.SecretKeyReference{
									Name: credentialTestHTTPSecretName,
									Key:  credentialTestTokenKey,
								},
							},
						},
					},
				},
			},
		}

		for _, credential := range credentials {
			Expect(k8sClient.Create(ctx, credential)).To(Succeed())
			Expect(k8sClient.Delete(ctx, credential)).To(Succeed())
		}
	})

	It("rejects an unsupported credential type", func() {
		credential := &configsv1alpha1.Credential{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "unsupported-type",
				Namespace: testNamespace,
			},
			Spec: configsv1alpha1.CredentialSpec{
				Type: "Password",
				HTTPBasicAuth: &configsv1alpha1.HTTPBasicAuthCredential{
					Username: "git",
					PasswordRef: configsv1alpha1.SecretKeyReference{
						Name: "password",
						Key:  "password",
					},
				},
			},
		}

		Expect(k8sClient.Create(ctx, credential)).NotTo(Succeed())
	})

	It("rejects a credential payload that does not match its type", func() {
		credential := &configsv1alpha1.Credential{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mismatched-payload",
				Namespace: testNamespace,
			},
			Spec: configsv1alpha1.CredentialSpec{
				Type: configsv1alpha1.CredentialTypeHTTPBearerToken,
				SSHPrivateKey: &configsv1alpha1.SSHPrivateKeyCredential{
					PrivateKeyRef: configsv1alpha1.SecretKeyReference{
						Name: credentialTestSSHSecretName,
						Key:  credentialTestSSHPrivateKeyKey,
					},
				},
			},
		}

		Expect(k8sClient.Create(ctx, credential)).NotTo(Succeed())
	})
})
