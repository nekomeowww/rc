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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

var _ = Describe("AgentProcess API validation", func() {
	It("allows a stop transition when optional execution fields are absent", func() {
		process := &workspacesv1alpha1.AgentProcess{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "validation-", Namespace: testAPINamespace},
			Spec: workspacesv1alpha1.AgentProcessSpec{
				TargetRef: workspacesv1alpha1.AgentProcessTargetReference{Kind: workspacesv1alpha1.AgentProcessTargetWorkspace, Name: "workspace"},
				Command:   []string{testTrueValue}, DesiredState: workspacesv1alpha1.AgentProcessDesiredStateRunning,
			},
		}
		Expect(k8sClient.Create(ctx, process)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, process) })

		current := new(workspacesv1alpha1.AgentProcess)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(process), current)).To(Succeed())
		current.Spec.DesiredState = workspacesv1alpha1.AgentProcessDesiredStateStopped
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
	})
})
