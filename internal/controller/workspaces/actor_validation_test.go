//go:build integration

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

package workspaces

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/apis/kubernetes/workspaces/v1alpha1"
)

var _ = Describe("Actor API validation", func() {
	actorWith := func(executor workspacesv1alpha1.ActorExecutor) *workspacesv1alpha1.Actor {
		return &workspacesv1alpha1.Actor{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "validation-", Namespace: testAPINamespace},
			Spec: workspacesv1alpha1.ActorSpec{
				Profile:   workspacesv1alpha1.ActorProfile{Builtin: "opencode"},
				Executors: []workspacesv1alpha1.ActorExecutor{executor},
			},
		}
	}

	It("admits an executor bound to a Workspace or to an external runner", func() {
		for _, executor := range []workspacesv1alpha1.ActorExecutor{
			{
				Name:         "ws",
				WorkspaceRef: &corev1.LocalObjectReference{Name: "dev"},
			},
			{
				Name:     "mac",
				External: &workspacesv1alpha1.ActorExternalExecutor{},
			},
		} {
			actor := actorWith(executor)
			Expect(k8sClient.Create(ctx, actor)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, actor) })

			stored := new(workspacesv1alpha1.Actor)
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(actor), stored)).To(Succeed())
			Expect(stored.Spec.Executors[0]).To(Equal(executor), "an empty external is kept")
		}
	})

	It("refuses an executor bound to both or to neither", func() {
		for _, executor := range []workspacesv1alpha1.ActorExecutor{
			{
				Name:         "both",
				WorkspaceRef: &corev1.LocalObjectReference{Name: "dev"},
				External:     &workspacesv1alpha1.ActorExternalExecutor{},
			},
			{Name: "neither"},
		} {
			err := k8sClient.Create(ctx, actorWith(executor))
			Expect(err).To(MatchError(ContainSubstring("exactly one of workspaceRef and external must be set")), executor.Name)
		}
	})
})
