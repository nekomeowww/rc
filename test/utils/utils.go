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

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	"github.com/onsi/gomega"
)

const (
	certmanagerVersion = "v1.20.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	for _, lease := range kubeSystemLeases {
		cmd = exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	// Suites live in nested directories under test/, so walk up to go.mod.
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			return wd, fmt.Errorf("failed to find go.mod above %q", wd)
		}
	}
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncommented", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}

// RunWithInput executes the provided command with input as its standard input.
func RunWithInput(cmd *exec.Cmd, input string) (string, error) {
	cmd.Stdin = strings.NewReader(input)
	return Run(cmd)
}

// KubectlApply applies the given manifest with kubectl.
func KubectlApply(manifest string) error {
	_, err := RunWithInput(exec.Command("kubectl", "apply", "-f", "-"), manifest)
	return err
}

// DeployManager deploys the manager like "make deploy" with the given images.
// "make deploy" rewrites config/manager/kustomization.yaml in place, so this
// sets the images on a temporary copy of config/ instead: the checkout is
// never modified, even when suites run concurrently or are interrupted.
func DeployManager(managerImage, runnerImage, rcKubeImage string) error {
	dir, err := GetProjectDir()
	if err != nil {
		return err
	}
	if _, err := Run(exec.Command("make", "manifests", "kustomize")); err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "rc-e2e-config-")
	if err != nil {
		return fmt.Errorf("failed to create a staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	config := filepath.Join(staging, "config")
	if err := os.CopyFS(config, os.DirFS(filepath.Join(dir, "config"))); err != nil {
		return fmt.Errorf("failed to copy config: %w", err)
	}

	kustomize := filepath.Join(dir, "bin", "kustomize")
	edit := exec.Command(kustomize, "edit", "set", "image",
		"controller="+managerImage,
		"runner="+runnerImage,
		"rc-kube="+rcKubeImage,
	)
	edit.Dir = filepath.Join(config, "manager")
	if output, err := edit.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set the manager images: %w: %s", err, output)
	}
	manifest, err := exec.Command(kustomize, "build", filepath.Join(config, "default")).Output()
	if err != nil {
		return fmt.Errorf("failed to build the manager manifests: %w", err)
	}
	_, err = RunWithInput(exec.Command("kubectl", "apply", "-f", "-"), string(manifest))
	return err
}

// CreateNamespace creates a namespace and succeeds when it already exists.
func CreateNamespace(name string) error {
	manifest := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name)
	return KubectlApply(manifest)
}

// Kubectl runs kubectl with the given arguments and returns its trimmed output.
func Kubectl(args ...string) (string, error) {
	output, err := Run(exec.Command("kubectl", args...))
	return strings.TrimSpace(output), err
}

// GetJSONPath returns one JSONPath expression of a namespaced resource.
func GetJSONPath(namespace, resource, name, jsonPath string) (string, error) {
	return Kubectl("get", resource, name, "-n", namespace, "-o", "jsonpath="+jsonPath)
}

// WorkspaceManifest renders a blank Workspace. extraSpec holds additional
// spec fields as YAML indented by two spaces, one field per line.
func WorkspaceManifest(namespace, name, image, storageClass, extraSpec string) string {
	return fmt.Sprintf(`apiVersion: workspaces.rc.ayaka.io/v1alpha1
kind: Workspace
metadata:
  name: %s
  namespace: %s
spec:
  image: %s
  automountServiceAccountToken: false
  storage:
    storageClassName: %s
    size: 1Gi
%s`, name, namespace, image, storageClass, extraSpec)
}

// DumpNamespaceDiagnostics writes the rc resources, Pods, and events of a
// namespace and the manager logs to the Ginkgo writer.
func DumpNamespaceDiagnostics(managerNamespace string, namespaces ...string) {
	logs, err := Kubectl("logs", "deployment/rc-controller-manager", "-n", managerNamespace,
		"-c", "manager",
		"--tail=400",
	)
	_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs (err=%v):\n%s\n", err, logs)
	for _, namespace := range namespaces {
		resources, err := Kubectl("get", "workspaces,workspaceexecs,pods,pvc", "-n", namespace, "-o", "yaml")
		_, _ = fmt.Fprintf(GinkgoWriter, "Resources in %s (err=%v):\n%s\n", namespace, err, resources)
		events, err := Kubectl("get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
		_, _ = fmt.Fprintf(GinkgoWriter, "Events in %s (err=%v):\n%s\n", namespace, err, events)
	}
}

// Field returns one JSONPath expression of a namespaced resource and fails
// the Gomega assertion g when kubectl fails.
func Field(g gomega.Gomega, namespace, resource, name, jsonPath string) string {
	output, err := GetJSONPath(namespace, resource, name, jsonPath)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return output
}

// ParseTime parses an RFC 3339 status time and fails g when it is malformed.
func ParseTime(g gomega.Gomega, value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "expected an RFC 3339 time, got %q", value)
	return parsed
}
