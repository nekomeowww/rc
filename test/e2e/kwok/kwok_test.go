//go:build e2e_kwok

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

package kwok

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nekomeowww/rc/test/utils"
)

const (
	workspaceNamespace = "rc-e2e-kwok"
	quotaNamespace     = "rc-e2e-kwok-quota"
	priorityClass      = "rc-e2e-kwok-runtime"

	// scaleCount Workspaces start and stop together in the scale spec.
	scaleCount = 30

	timeoutShort = time.Minute
	timeoutLong  = 3 * time.Minute
	pollInterval = time.Second
	clockSkew    = 3 * time.Second
)

// placement schedules a runtime Pod to the fake Node only.
var placement = fmt.Sprintf(`  nodeSelector:
    %s: %s
  tolerations:
    - key: %s
      operator: Exists
      effect: NoSchedule
`, fakeNodeLabel, fakeNodeValue, fakeNodeTaint)

var _ = Describe("Workspaces on kwok Nodes", Ordered, func() {
	BeforeAll(func() {
		Expect(utils.CreateNamespace(workspaceNamespace)).To(Succeed())
		Expect(utils.CreateNamespace(quotaNamespace)).To(Succeed())
	})

	AfterAll(func() {
		for _, ns := range []string{workspaceNamespace, quotaNamespace} {
			By("deleting the Workspaces in " + ns)
			_, _ = utils.Kubectl("delete", "workspaces", "--all", "-n", ns, "--timeout=3m")
			_, _ = utils.Kubectl("delete", "ns", ns, "--ignore-not-found", "--timeout=3m")
		}
		_, _ = utils.Kubectl("delete", "priorityclass", priorityClass, "--ignore-not-found")
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpNamespaceDiagnostics(managerNamespace, workspaceNamespace, quotaNamespace)
		}
	})

	Context("runtime Pod placement and metadata", Ordered, func() {
		const name = "placed"

		BeforeAll(func() {
			Expect(utils.KubectlApply(fmt.Sprintf(`apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: %s
value: 1000
globalDefault: false
`, priorityClass))).To(Succeed())
			Expect(utils.KubectlApply(utils.WorkspaceManifest(workspaceNamespace, name, fakeImage, storageClassName,
				placement+`  podMetadata:
    labels:
      e2e.example.com/team: alpha
      e2e.example.com/removed: "yes"
`))).To(Succeed())
		})

		It("schedules the runtime Pod on a fake Node and reports Ready", func() {
			waitReady(workspaceNamespace, name, timeoutShort)
			pod := podName(name)
			Expect(utils.GetJSONPath(workspaceNamespace, "pod", pod, "{.spec.nodeName}")).To(Equal(fakeNodeName))
			Expect(utils.GetJSONPath(workspaceNamespace, "pod", pod,
				"{.metadata.labels.e2e\\.example\\.com/team}")).To(Equal("alpha"))
		})

		It("patches runtime Pod labels in place", func() {
			uid := podUID(name)
			_, err := utils.Kubectl("patch", "workspace", name, "-n", workspaceNamespace, "--type=merge",
				"-p", `{"spec":{"podMetadata":{"labels":{"e2e.example.com/team":"beta","e2e.example.com/removed":null}}}}`)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				labels := utils.Field(g, workspaceNamespace, "pod", podName(name), "{.metadata.labels}")
				g.Expect(labels).To(ContainSubstring(`"e2e.example.com/team":"beta"`))
				g.Expect(labels).NotTo(ContainSubstring("e2e.example.com/removed"))
			}).WithTimeout(timeoutShort).WithPolling(pollInterval).Should(Succeed())
			Expect(podUID(name)).To(Equal(uid), "metadata must not replace the Pod")
		})

		It("replaces the runtime Pod for priorityClassName and keeps runningSince", func() {
			uid := podUID(name)
			runningSince := text(name, "{.status.runningSince}")
			Expect(runningSince).NotTo(BeEmpty())

			_, err := utils.Kubectl("patch", "workspace", name, "-n", workspaceNamespace, "--type=merge",
				"-p", fmt.Sprintf(`{"spec":{"priorityClassName":%q}}`, priorityClass))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				pod := utils.Field(g, workspaceNamespace, "workspace", name, "{.status.runtimePodName}")
				g.Expect(utils.Field(g, workspaceNamespace, "pod", pod, "{.metadata.uid}")).NotTo(Equal(uid))
				g.Expect(utils.Field(g, workspaceNamespace, "pod", pod, "{.spec.priorityClassName}")).To(Equal(priorityClass))
				g.Expect(readyStatus(g, workspaceNamespace, name)).To(Equal("True"))
			}).WithTimeout(timeoutShort).WithPolling(pollInterval).Should(Succeed())
			Expect(text(name, "{.status.runningSince}")).To(Equal(runningSince))
		})
	})

	It("reports AdmissionRejected and recovers after the quota is raised", func() {
		const name = "quota"
		Expect(applyPodQuota(0)).To(Succeed())
		Expect(utils.KubectlApply(utils.WorkspaceManifest(quotaNamespace, name, fakeImage, storageClassName,
			placement))).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(utils.Field(g, quotaNamespace, "workspace", name,
				"{.status.conditions[?(@.type=='Ready')].reason}")).To(Equal("AdmissionRejected"))
			g.Expect(utils.Field(g, quotaNamespace, "workspace", name,
				"{.status.conditions[?(@.type=='Ready')].message}")).To(ContainSubstring("exceeded quota"))
		}).WithTimeout(timeoutShort).WithPolling(pollInterval).Should(Succeed())

		Expect(applyPodQuota(1)).To(Succeed())
		// The retry backoff is at most one minute.
		waitReady(quotaNamespace, name, timeoutShort+timeoutShort)
	})

	It("keeps an idle Workspace running when it cannot read runtime activity, until maxRunDuration", func() {
		// No supervisor answers Pod exec on a kwok Node. The idle path fails
		// closed on that error, so only the maximum run stops this Workspace.
		const (
			name        = "unreachable"
			idleTimeout = 15 * time.Second
			maxRun      = 60 * time.Second
		)
		Expect(utils.KubectlApply(utils.WorkspaceManifest(workspaceNamespace, name, fakeImage, storageClassName,
			placement+fmt.Sprintf("  idleTimeout: %s\n  maxRunDuration: %s\n", idleTimeout, maxRun)))).To(Succeed())
		waitReady(workspaceNamespace, name, timeoutShort)
		runningSince := utils.ParseTime(Default, text(name, "{.status.runningSince}"))

		By("checking the Workspace stays running past its idle deadline")
		Consistently(func(g Gomega) {
			g.Expect(utils.Field(g, workspaceNamespace, "workspace", name, "{.spec.desiredState}")).To(Equal("Running"))
		}).WithTimeout(max(time.Until(runningSince.Add(maxRun-10*time.Second)), 5*time.Second)).
			WithPolling(pollInterval).Should(Succeed())

		By("waiting for the maximum-run suspension")
		Eventually(func(g Gomega) {
			g.Expect(utils.Field(g, workspaceNamespace, "workspace", name, "{.spec.desiredState}")).To(Equal("Suspended"))
		}).WithTimeout(timeoutLong).WithPolling(pollInterval).Should(Succeed())
		Expect(time.Now()).To(BeTemporally(">=", runningSince.Add(maxRun-clockSkew)))
		Expect(text(name, "{.status.lifecycle.lastAutomaticSuspendReason}")).To(Equal("MaxRunDuration"))
		waitSuspended(workspaceNamespace, name)
	})

	It("starts, suspends, and deletes many Workspaces at once", func() {
		const maxRun = 45 * time.Second
		var manifest strings.Builder
		for index := range scaleCount {
			if index > 0 {
				manifest.WriteString("---\n")
			}
			workspace := utils.WorkspaceManifest(workspaceNamespace, scaleName(index), fakeImage, storageClassName,
				placement+fmt.Sprintf("  maxRunDuration: %s\n", maxRun))
			manifest.WriteString(strings.Replace(workspace, "metadata:\n",
				"metadata:\n  labels:\n    e2e.example.com/scale: \"true\"\n", 1))
		}
		started := time.Now()
		Expect(utils.KubectlApply(manifest.String())).To(Succeed())

		By(fmt.Sprintf("waiting for %d Workspaces to become Ready", scaleCount))
		Eventually(func(g Gomega) {
			g.Expect(countScaleWorkspaces(g, "{.status.conditions[?(@.type=='Ready')].status}", "True")).
				To(Equal(scaleCount))
		}).WithTimeout(timeoutLong).WithPolling(2 * pollInterval).Should(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "%d Workspaces became Ready in %s\n", scaleCount, time.Since(started))

		By("waiting for every Workspace to suspend at its maximum run duration")
		Eventually(func(g Gomega) {
			g.Expect(countScaleWorkspaces(g, "{.status.conditions[?(@.type=='Ready')].reason}", "Suspended")).
				To(Equal(scaleCount))
			g.Expect(countScaleWorkspaces(g, "{.status.lifecycle.lastAutomaticSuspendReason}", "MaxRunDuration")).
				To(Equal(scaleCount))
		}).WithTimeout(maxRun + timeoutLong).WithPolling(2 * pollInterval).Should(Succeed())

		By("deleting the Workspaces")
		_, err := utils.Kubectl("delete", "workspaces", "-n", workspaceNamespace, "-l", "e2e.example.com/scale=true",
			"--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
	})
})

func scaleName(index int) string {
	return fmt.Sprintf("scale-%02d", index)
}

// countScaleWorkspaces counts the scale Workspaces whose jsonPath equals want.
func countScaleWorkspaces(g Gomega, jsonPath, want string) int {
	output, err := utils.Kubectl("get", "workspaces", "-n", workspaceNamespace, "-l", "e2e.example.com/scale=true",
		"-o", "jsonpath={range .items[*]}"+jsonPath+"{\"\\n\"}{end}")
	g.Expect(err).NotTo(HaveOccurred())
	count := 0
	for line := range strings.SplitSeq(output, "\n") {
		if line == want {
			count++
		}
	}
	return count
}

func applyPodQuota(pods int) error {
	return utils.KubectlApply(fmt.Sprintf(`apiVersion: v1
kind: ResourceQuota
metadata:
  name: rc-e2e-pods
  namespace: %s
spec:
  hard:
    pods: "%d"
`, quotaNamespace, pods))
}

func readyStatus(g Gomega, ns, name string) string {
	return utils.Field(g, ns, "workspace", name, "{.status.conditions[?(@.type=='Ready')].status}")
}

func waitReady(ns, name string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(readyStatus(g, ns, name)).To(Equal("True"))
		g.Expect(utils.Field(g, ns, "workspace", name, "{.status.runningSince}")).NotTo(BeEmpty())
	}).WithTimeout(timeout).WithPolling(pollInterval).Should(Succeed())
}

func waitSuspended(ns, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(utils.Field(g, ns, "workspace", name,
			"{.status.conditions[?(@.type=='Ready')].reason}")).To(Equal("Suspended"))
	}).WithTimeout(timeoutShort).WithPolling(pollInterval).Should(Succeed())
}

func text(name, jsonPath string) string {
	GinkgoHelper()
	output, err := utils.GetJSONPath(workspaceNamespace, "workspace", name, jsonPath)
	Expect(err).NotTo(HaveOccurred())
	return output
}

func podName(name string) string {
	GinkgoHelper()
	pod := text(name, "{.status.runtimePodName}")
	Expect(pod).NotTo(BeEmpty())
	return pod
}

func podUID(name string) string {
	GinkgoHelper()
	uid, err := utils.GetJSONPath(workspaceNamespace, "pod", podName(name), "{.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())
	return uid
}
