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

// Package kwok runs controller-level e2e specs against Workspace runtime Pods
// placed on kwok fake Nodes. kwok marks those Pods Running and Ready without
// starting a container, so nothing here can exec into a runtime: specs that
// need rc-kube belong to the Kind suite in test/e2e.
package kwok

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nekomeowww/rc/test/utils"
)

const (
	managerImage     = "example.com/rc:v0.0.1"
	managerNamespace = "rc-system"
	// fakeImage is never pulled: no container runs on a kwok Node.
	fakeImage = "registry.invalid/rc/fake-runtime:v1"
	// rcKubeImage is built with the manager but never pulled either.
	rcKubeImage = "example.com/rc-kube:v0.0.1"

	fakeNodeName  = "kwok-node-0"
	fakeNodeLabel = "type"
	fakeNodeValue = "kwok"
	fakeNodeTaint = "kwok.x-k8s.io/node"

	// storageClassName binds pre-created PersistentVolumes without a
	// provisioner. They have no node affinity, so a Pod using one can be
	// scheduled to a fake Node; the CSI hostpath driver pins its volumes to
	// the real Kind Node instead.
	storageClassName = "rc-e2e-kwok"
	// volumeCount bounds how many Workspaces the suite creates in total.
	volumeCount = 48
)

func TestKwok(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting rc kwok e2e test suite\n")
	RunSpecs(t, "kwok e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage), fmt.Sprintf("RC_KUBE_IMG=%s", rcKubeImage))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())

	By("checking that kwok is installed")
	_, err = utils.Kubectl("rollout", "status", "deployment/kwok-controller", "-n", "kube-system", "--timeout=2m")
	Expect(err).NotTo(HaveOccurred(), "kwok is not installed; run make setup-kwok")

	By("creating a fake Node")
	Expect(utils.KubectlApply(fakeNodeManifest())).To(Succeed())
	Eventually(func(g Gomega) {
		g.Expect(utils.Kubectl("get", "node", fakeNodeName,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")).To(Equal("True"))
	}).WithTimeout(timeoutLong).WithPolling(pollInterval).Should(Succeed())

	By("creating static volumes for Workspace homes")
	Expect(utils.KubectlApply(volumeManifests())).To(Succeed())

	By("deploying the controller-manager")
	Expect(utils.CreateNamespace(managerNamespace)).To(Succeed())
	_, err = utils.Run(exec.Command("make", "install"))
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")
	// The runner image is never pulled on kwok Nodes; every Workspace sets image.
	Expect(utils.DeployManager(managerImage, fakeImage, rcKubeImage)).To(Succeed())
	_, err = utils.Kubectl("rollout", "status", "deployment/rc-controller-manager", "-n", managerNamespace,
		"--timeout=3m")
	Expect(err).NotTo(HaveOccurred(), "Controller-manager did not become available")
})

var _ = AfterSuite(func() {
	By("undeploying the controller-manager")
	_, _ = utils.Run(exec.Command("make", "undeploy", "ignore-not-found=true"))
	_, _ = utils.Run(exec.Command("make", "uninstall", "ignore-not-found=true"))
	_, _ = utils.Kubectl("delete", "ns", managerNamespace, "--ignore-not-found")

	By("removing the fake Node and static volumes")
	_, _ = utils.Kubectl("delete", "node", fakeNodeName, "--ignore-not-found")
	_, _ = utils.Kubectl("delete", "pv", "-l", "e2e.example.com/kwok=true", "--ignore-not-found")
	_, _ = utils.Kubectl("delete", "storageclass", storageClassName, "--ignore-not-found")
})

func fakeNodeManifest() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Node
metadata:
  name: %[1]s
  annotations:
    kwok.x-k8s.io/node: fake
    node.alpha.kubernetes.io/ttl: "0"
  labels:
    kubernetes.io/arch: amd64
    kubernetes.io/hostname: %[1]s
    kubernetes.io/os: linux
    kubernetes.io/role: agent
    node-role.kubernetes.io/agent: ""
    %[2]s: %[3]s
spec:
  taints:
    - key: %[4]s
      value: fake
      effect: NoSchedule
status:
  allocatable:
    cpu: "64"
    memory: 256Gi
    pods: "250"
  capacity:
    cpu: "64"
    memory: 256Gi
    pods: "250"
  nodeInfo:
    architecture: amd64
    operatingSystem: linux
    kubeletVersion: fake
    kubeProxyVersion: fake
`, fakeNodeName, fakeNodeLabel, fakeNodeValue, fakeNodeTaint)
}

func volumeManifests() string {
	var manifest strings.Builder
	fmt.Fprintf(&manifest, `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: kubernetes.io/no-provisioner
volumeBindingMode: Immediate
reclaimPolicy: Retain
`, storageClassName)
	for index := range volumeCount {
		fmt.Fprintf(&manifest, `---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: rc-e2e-kwok-%02d
  labels:
    e2e.example.com/kwok: "true"
spec:
  storageClassName: %s
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  hostPath:
    path: /tmp/rc-e2e-kwok/%02d
`, index, storageClassName, index)
	}
	return manifest.String()
}
