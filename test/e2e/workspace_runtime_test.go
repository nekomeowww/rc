//go:build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nekomeowww/rc/test/utils"
)

const (
	// workspaceNamespace holds the runtime Workspaces of this suite.
	workspaceNamespace = "rc-e2e-workspaces"
	// quotaNamespace holds a ResourceQuota that rejects runtime Pods.
	quotaNamespace = "rc-e2e-quota"
	// workspaceStorageClass is installed by "make setup-kind".
	workspaceStorageClass = "csi-hostpath-sc"
	// runtimePriorityClass is created by this suite.
	runtimePriorityClass = "rc-e2e-runtime"

	// The clocks are short so the suite stays fast. Every wait below adds
	// slack for Pod start, the retention controller's requeue, and polling.
	quietIdleTimeout  = 30 * time.Second
	activeIdleTimeout = 40 * time.Second
	maxRunDuration    = 90 * time.Second
	// clockSkew tolerates second-granularity status times and polling.
	clockSkew = 3 * time.Second

	readyTimeout     = 3 * time.Minute
	transitionWindow = 2 * time.Minute
	pollInterval     = 2 * time.Second
)

// processEntry is the part of "rc-kube process list" output the specs read.
type processEntry struct {
	ID        string     `json:"id"`
	Phase     string     `json:"phase"`
	Command   []string   `json:"command"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

var _ = Describe("Workspace runtime", Ordered, func() {
	BeforeAll(func() {
		By("creating the Workspace namespaces")
		Expect(utils.CreateNamespace(workspaceNamespace)).To(Succeed())
		Expect(utils.CreateNamespace(quotaNamespace)).To(Succeed())
	})

	AfterAll(func() {
		for _, ns := range []string{workspaceNamespace, quotaNamespace} {
			By("deleting the Workspaces in " + ns)
			_, _ = utils.Kubectl("delete", "workspaceexecs", "--all", "-n", ns, "--timeout=2m")
			_, _ = utils.Kubectl("delete", "workspaces", "--all", "-n", ns, "--timeout=3m")
			_, _ = utils.Kubectl("delete", "ns", ns, "--ignore-not-found", "--timeout=3m")
		}
		_, _ = utils.Kubectl("delete", "priorityclass", runtimePriorityClass, "--ignore-not-found")
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			utils.DumpNamespaceDiagnostics(namespace, workspaceNamespace, quotaNamespace)
		}
	})

	Context("idle suspension", Ordered, func() {
		const (
			quietName  = "idle-quiet"
			activeName = "idle-active"
		)
		var (
			quietRunningSince time.Time
			quietSuspendedBy  time.Time
			quietPodUID       string
		)

		BeforeAll(func() {
			By("creating a Workspace whose service only sleeps")
			Expect(utils.KubectlApply(utils.WorkspaceManifest(workspaceNamespace, quietName, runnerImage,
				workspaceStorageClass, fmt.Sprintf(`  idleTimeout: %s
  services:
    - name: quiet
      command: ["sh", "-c", "sleep 3600"]
`, quietIdleTimeout)))).To(Succeed())

			By("creating a Workspace whose service reports activity")
			Expect(utils.KubectlApply(utils.WorkspaceManifest(workspaceNamespace, activeName, runnerImage,
				workspaceStorageClass, fmt.Sprintf(`  idleTimeout: %s
  services:
    - name: agent
      # rc-kube is not in the image. The injected rc-kube puts itself on
      # PATH for the processes it starts.
      command: ["sh", "-c", "while true; do rc-kube process touch; sleep 10; done"]
`, activeIdleTimeout)))).To(Succeed())
		})

		It("suspends an idle Workspace whose resident service does not report activity", func() {
			waitWorkspaceReady(quietName)
			quietRunningSince = workspaceTime(quietName, "{.status.runningSince}")
			quietPodUID = runtimePodUID(quietName)

			By("listing the resident service run in the supervisor")
			Eventually(func(g Gomega) {
				service := findService(g, runtimeProcesses(g, workspaceNamespace, quietName))
				g.Expect(service.Phase).To(Equal("Running"))
				g.Expect(service.Command).To(ContainElement(ContainSubstring("sleep 3600")))
			}, time.Minute, pollInterval).Should(Succeed())

			By("waiting for the idle suspension")
			Eventually(func(g Gomega) {
				g.Expect(workspaceField(g, workspaceNamespace, quietName, "{.spec.desiredState}")).To(Equal("Suspended"))
			}, quietIdleTimeout+transitionWindow, pollInterval).Should(Succeed())
			quietSuspendedBy = time.Now()
			Expect(quietSuspendedBy).To(BeTemporally(">=", quietRunningSince.Add(quietIdleTimeout-clockSkew)),
				"the Workspace suspended before its idle timeout")
			Expect(workspaceText(workspaceNamespace, quietName,
				"{.status.lifecycle.lastAutomaticSuspendReason}")).To(Equal("Idle"))

			By("waiting for the runtime to stop")
			waitWorkspaceSuspended(workspaceNamespace, quietName)
		})

		It("keeps a Workspace running while its service reports activity", func() {
			waitWorkspaceReady(activeName)
			runningSince := workspaceTime(activeName, "{.status.runningSince}")

			// Two idle timeouts give the controller two deadlines at which it must
			// read the supervisor's activity and postpone.
			stayAwakeUntil := runningSince.Add(2*activeIdleTimeout + 10*time.Second)
			By("checking the Workspace stays running past two idle deadlines")
			Consistently(func(g Gomega) {
				g.Expect(workspaceField(g, workspaceNamespace, activeName, "{.spec.desiredState}")).To(Equal("Running"))
			}, max(time.Until(stayAwakeUntil), 10*time.Second), pollInterval).Should(Succeed())

			By("checking the reported activity advanced the idle clock")
			lastActivity := workspaceTime(activeName, "{.status.lastActivityTime}")
			Expect(lastActivity).To(BeTemporally(">", runningSince.Add(activeIdleTimeout/2)))
			Expect(workspaceText(workspaceNamespace, activeName,
				"{.status.lifecycle.lastAutomaticSuspendReason}")).To(BeEmpty())
		})

		It("restarts the resident service and resets runningSince on resume", func() {
			Expect(quietRunningSince.IsZero()).To(BeFalse(), "the idle suspension spec must run first")

			By("resuming the suspended Workspace")
			_, err := utils.Kubectl("patch", "workspace", quietName, "-n", workspaceNamespace, "--type=merge",
				"-p", `{"spec":{"desiredState":"Running"}}`)
			Expect(err).NotTo(HaveOccurred())

			waitWorkspaceReady(quietName)
			resumedSince := workspaceTime(quietName, "{.status.runningSince}")
			Expect(resumedSince).To(BeTemporally(">", quietRunningSince), "a resume must start a new run")
			Expect(runtimePodUID(quietName)).NotTo(Equal(quietPodUID))

			By("checking the supervisor started the service again")
			Eventually(func(g Gomega) {
				service := findService(g, runtimeProcesses(g, workspaceNamespace, quietName))
				g.Expect(service.Phase).To(Equal("Running"))
				g.Expect(service.StartedAt).NotTo(BeNil())
				// Records are durable in the home volume; this run must be new.
				g.Expect(*service.StartedAt).To(BeTemporally(">", quietSuspendedBy))
			}, time.Minute, pollInterval).Should(Succeed())
		})
	})

	Context("maximum run duration", Ordered, func() {
		const (
			workspaceName = "bounded"
			execName      = "bounded-sleep"
		)

		BeforeAll(func() {
			By("creating a Workspace with a maximum run duration and a long execution")
			Expect(utils.KubectlApply(utils.WorkspaceManifest(workspaceNamespace, workspaceName, runnerImage,
				workspaceStorageClass, fmt.Sprintf("  maxRunDuration: %s\n", maxRunDuration)))).To(Succeed())
			Expect(utils.KubectlApply(fmt.Sprintf(`apiVersion: workspaces.rc.ayaka.io/v1alpha1
kind: WorkspaceExec
metadata:
  name: %s
  namespace: %s
spec:
  targetRef:
    kind: Workspace
    name: %s
  command: ["sleep", "3600"]
`, execName, workspaceNamespace, workspaceName))).To(Succeed())
		})

		It("stops running executions and suspends with the MaxRunDuration reason", func() {
			waitWorkspaceReady(workspaceName)
			runningSince := workspaceTime(workspaceName, "{.status.runningSince}")

			By("waiting for the execution to run")
			Eventually(func(g Gomega) {
				g.Expect(workspaceExecField(g, execName, "{.status.phase}")).To(Equal("Running"))
			}, transitionWindow, pollInterval).Should(Succeed())

			By("checking the published maximum-run deadline")
			Eventually(func(g Gomega) {
				deadline := utils.ParseTime(g, workspaceField(g, workspaceNamespace, workspaceName,
					"{.status.lifecycle.maxRunSuspendAt}"))
				g.Expect(deadline).To(BeTemporally("~", runningSince.Add(maxRunDuration), clockSkew))
			}, time.Minute, pollInterval).Should(Succeed())

			By("waiting for the maximum-run suspension")
			Eventually(func(g Gomega) {
				g.Expect(workspaceField(g, workspaceNamespace, workspaceName, "{.spec.desiredState}")).To(Equal("Suspended"))
			}, maxRunDuration+transitionWindow, pollInterval).Should(Succeed())
			Expect(time.Now()).To(BeTemporally(">=", runningSince.Add(maxRunDuration-clockSkew)),
				"the Workspace suspended before its maximum run duration")
			Expect(workspaceText(workspaceNamespace, workspaceName,
				"{.status.lifecycle.lastAutomaticSuspendReason}")).To(Equal("MaxRunDuration"))

			By("checking the execution was stopped")
			Eventually(func(g Gomega) {
				g.Expect(workspaceExecField(g, execName, "{.spec.desiredState}")).To(Equal("Stopped"))
				g.Expect(workspaceExecField(g, execName, "{.status.phase}")).To(Equal("Stopped"))
			}, transitionWindow, pollInterval).Should(Succeed())

			waitWorkspaceSuspended(workspaceNamespace, workspaceName)
		})
	})

	Context("runtime Pod metadata and priority", Ordered, func() {
		const workspaceName = "metadata"

		BeforeAll(func() {
			By("creating a PriorityClass for runtime Pods")
			Expect(utils.KubectlApply(fmt.Sprintf(`apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: %s
value: 1000
globalDefault: false
`, runtimePriorityClass))).To(Succeed())

			By("creating a Workspace with runtime Pod metadata")
			Expect(utils.KubectlApply(utils.WorkspaceManifest(workspaceNamespace, workspaceName, runnerImage,
				workspaceStorageClass, `  podMetadata:
    labels:
      e2e.example.com/team: alpha
      e2e.example.com/removed: "yes"
    annotations:
      e2e.example.com/note: first
`))).To(Succeed())
		})

		It("patches labels and annotations onto the running Pod in place", func() {
			waitWorkspaceReady(workspaceName)
			podUID := runtimePodUID(workspaceName)
			Expect(podLabel(workspaceNamespace, workspaceName, "e2e\\.example\\.com/team")).To(Equal("alpha"))
			Expect(podAnnotation(workspaceNamespace, workspaceName, "e2e\\.example\\.com/note")).To(Equal("first"))

			By("changing, adding, and removing metadata keys")
			_, err := utils.Kubectl("patch", "workspace", workspaceName, "-n", workspaceNamespace, "--type=merge",
				"-p", `{"spec":{"podMetadata":{`+
					`"labels":{"e2e.example.com/team":"beta","e2e.example.com/removed":null},`+
					`"annotations":{"e2e.example.com/note":null,"e2e.example.com/added":"second"}}}}`)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				labels := podMap(g, workspaceNamespace, workspaceName, "{.metadata.labels}")
				g.Expect(labels).To(HaveKeyWithValue("e2e.example.com/team", "beta"))
				g.Expect(labels).NotTo(HaveKey("e2e.example.com/removed"))
				annotations := podMap(g, workspaceNamespace, workspaceName, "{.metadata.annotations}")
				g.Expect(annotations).To(HaveKeyWithValue("e2e.example.com/added", "second"))
				g.Expect(annotations).NotTo(HaveKey("e2e.example.com/note"))
			}, time.Minute, pollInterval).Should(Succeed())

			Expect(runtimePodUID(workspaceName)).To(Equal(podUID), "metadata must not replace the Pod")
			Expect(workspaceText(workspaceNamespace, workspaceName,
				"{.status.conditions[?(@.type=='Ready')].status}")).To(Equal("True"))
		})

		It("replaces an idle runtime Pod for a new priorityClassName and keeps runningSince", func() {
			waitWorkspaceReady(workspaceName)
			podUID := runtimePodUID(workspaceName)
			runningSince := workspaceText(workspaceNamespace, workspaceName, "{.status.runningSince}")
			Expect(runningSince).NotTo(BeEmpty())

			By("setting priorityClassName")
			_, err := utils.Kubectl("patch", "workspace", workspaceName, "-n", workspaceNamespace, "--type=merge",
				"-p", fmt.Sprintf(`{"spec":{"priorityClassName":%q}}`, runtimePriorityClass))
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				pod := workspaceField(g, workspaceNamespace, workspaceName, "{.status.runtimePodName}")
				uid, err := utils.GetJSONPath(workspaceNamespace, "pod", pod, "{.metadata.uid}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(uid).NotTo(Equal(podUID))
				priority, err := utils.GetJSONPath(workspaceNamespace, "pod", pod, "{.spec.priorityClassName}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(priority).To(Equal(runtimePriorityClass))
				g.Expect(workspaceField(g, workspaceNamespace, workspaceName,
					"{.status.conditions[?(@.type=='Ready')].status}")).To(Equal("True"))
			}, readyTimeout, pollInterval).Should(Succeed())

			Expect(workspaceText(workspaceNamespace, workspaceName, "{.status.runningSince}")).To(Equal(runningSince),
				"a runtime Pod replacement must not start a new run")
			Expect(podLabel(workspaceNamespace, workspaceName, "e2e\\.example\\.com/team")).To(Equal("beta"))
		})
	})

	Context("admission rejection", Ordered, func() {
		const (
			workspaceName = "quota"
			quotaName     = "rc-e2e-pods"
		)

		BeforeAll(func() {
			By("creating a ResourceQuota that admits no Pods")
			Expect(applyPodQuota(quotaNamespace, quotaName, 0)).To(Succeed())
		})

		It("reports AdmissionRejected and recovers after the quota is raised", func() {
			Expect(utils.KubectlApply(utils.WorkspaceManifest(quotaNamespace, workspaceName, runnerImage,
				workspaceStorageClass, ""))).To(Succeed())

			By("waiting for the rejected runtime Pod to be reported")
			Eventually(func(g Gomega) {
				g.Expect(workspaceField(g, quotaNamespace, workspaceName,
					"{.status.conditions[?(@.type=='Ready')].reason}")).To(Equal("AdmissionRejected"))
				g.Expect(workspaceField(g, quotaNamespace, workspaceName,
					"{.status.conditions[?(@.type=='Ready')].message}")).To(ContainSubstring("exceeded quota"))
			}, readyTimeout, pollInterval).Should(Succeed())

			By("raising the quota")
			Expect(applyPodQuota(quotaNamespace, quotaName, 1)).To(Succeed())

			// The retry backoff is at most one minute.
			waitWorkspaceReadyIn(quotaNamespace, workspaceName, time.Minute+readyTimeout)
		})
	})
})

func applyPodQuota(ns, name string, pods int) error {
	return utils.KubectlApply(fmt.Sprintf(`apiVersion: v1
kind: ResourceQuota
metadata:
  name: %s
  namespace: %s
spec:
  hard:
    pods: "%d"
`, name, ns, pods))
}

func waitWorkspaceReady(name string) {
	GinkgoHelper()
	waitWorkspaceReadyIn(workspaceNamespace, name, readyTimeout)
}

func waitWorkspaceReadyIn(ns, name string, timeout time.Duration) {
	GinkgoHelper()
	By("waiting for Workspace " + name + " to become Ready")
	Eventually(func(g Gomega) {
		g.Expect(workspaceField(g, ns, name, "{.status.conditions[?(@.type=='Ready')].status}")).To(Equal("True"))
		g.Expect(workspaceField(g, ns, name, "{.status.runningSince}")).NotTo(BeEmpty())
	}, timeout, pollInterval).Should(Succeed())
}

func waitWorkspaceSuspended(ns, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(workspaceField(g, ns, name, "{.status.conditions[?(@.type=='Ready')].reason}")).To(Equal("Suspended"))
		g.Expect(workspaceField(g, ns, name, "{.status.runningSince}")).To(BeEmpty())
	}, transitionWindow, pollInterval).Should(Succeed())
}

func workspaceField(g Gomega, ns, name, jsonPath string) string {
	return utils.Field(g, ns, "workspace", name, jsonPath)
}

func workspaceText(ns, name, jsonPath string) string {
	GinkgoHelper()
	output, err := utils.GetJSONPath(ns, "workspace", name, jsonPath)
	Expect(err).NotTo(HaveOccurred())
	return output
}

func workspaceTime(name, jsonPath string) time.Time {
	GinkgoHelper()
	return utils.ParseTime(Default, workspaceText(workspaceNamespace, name, jsonPath))
}

func workspaceExecField(g Gomega, name, jsonPath string) string {
	return utils.Field(g, workspaceNamespace, "workspaceexec", name, jsonPath)
}

func runtimePodName(ns, name string) string {
	GinkgoHelper()
	pod := workspaceText(ns, name, "{.status.runtimePodName}")
	Expect(pod).NotTo(BeEmpty())
	return pod
}

func runtimePodUID(name string) string {
	GinkgoHelper()
	uid, err := utils.GetJSONPath(workspaceNamespace, "pod", runtimePodName(workspaceNamespace, name), "{.metadata.uid}")
	Expect(err).NotTo(HaveOccurred())
	return uid
}

func podLabel(ns, name, escapedKey string) string {
	GinkgoHelper()
	value, err := utils.GetJSONPath(ns, "pod", runtimePodName(ns, name), "{.metadata.labels."+escapedKey+"}")
	Expect(err).NotTo(HaveOccurred())
	return value
}

func podAnnotation(ns, name, escapedKey string) string {
	GinkgoHelper()
	value, err := utils.GetJSONPath(ns, "pod", runtimePodName(ns, name), "{.metadata.annotations."+escapedKey+"}")
	Expect(err).NotTo(HaveOccurred())
	return value
}

func podMap(g Gomega, ns, name, jsonPath string) map[string]string {
	pod := workspaceField(g, ns, name, "{.status.runtimePodName}")
	output, err := utils.GetJSONPath(ns, "pod", pod, jsonPath)
	g.Expect(err).NotTo(HaveOccurred())
	values := map[string]string{}
	if output != "" {
		g.Expect(json.Unmarshal([]byte(output), &values)).To(Succeed())
	}
	return values
}

// runtimeProcesses lists the supervisor's process records through Pod exec,
// the same bridge the controller uses. The runner image has no rc-kube, so
// this also checks that the controller injected one.
func runtimeProcesses(g Gomega, ns, name string) []processEntry {
	pod := workspaceField(g, ns, name, "{.status.runtimePodName}")
	output, err := utils.Kubectl("exec", pod, "-n", ns, "-c", "rc-kube", "--", "/opt/rc/bin/rc-kube", "process", "list")
	g.Expect(err).NotTo(HaveOccurred())
	entries := []processEntry{}
	g.Expect(json.Unmarshal([]byte(output), &entries)).To(Succeed(), "unexpected process list %q", output)
	return entries
}

func findService(g Gomega, entries []processEntry) processEntry {
	for _, entry := range entries {
		if strings.HasPrefix(entry.ID, "service_") && entry.Phase == "Running" {
			return entry
		}
	}
	g.Expect(entries).To(ContainElement(HaveField("ID", HavePrefix("service_"))), "no running service record")
	return processEntry{}
}
