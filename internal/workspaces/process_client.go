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
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const WorkspaceHomePath = "/home/agent"

type ProcessStartRequest struct {
	Namespace        string
	Target           workspacesv1alpha1.AgentProcessTargetReference
	Command          []string
	WorkingDirectory string
	TTY              bool
	AgentType        string
	AgentCredential  string
	Credentials      []string
	Environment      map[string]string
	NamePrefix       string
}

type ProcessClient struct {
	Kube          client.Client
	Runtime       *processruntime.KubeRuntime
	Config        *rest.Config
	NameGenerator func(string) string
}

type ProcessExitError struct {
	Process string
	Code    int32
}

func (err *ProcessExitError) Error() string {
	return fmt.Sprintf("AgentProcess %s exited with status %d", err.Process, err.Code)
}

func (err *ProcessExitError) ExitCode() int {
	return int(err.Code)
}

type ProcessTerminationError struct {
	Process string
	Phase   workspacesv1alpha1.AgentProcessPhase
	Reason  string
}

func (err *ProcessTerminationError) Error() string {
	if err.Reason != "" {
		return fmt.Sprintf("AgentProcess %s ended in phase %s: %s", err.Process, err.Phase, err.Reason)
	}
	return fmt.Sprintf("AgentProcess %s ended in phase %s", err.Process, err.Phase)
}

func (err *ProcessTerminationError) ExitCode() int { return 1 }

// ResultError translates every non-success terminal state into a CLI error.
func ResultError(process *workspacesv1alpha1.AgentProcess) error {
	if process.Status.Phase == workspacesv1alpha1.AgentProcessPhaseSucceeded {
		return nil
	}
	if process.Status.ExitCode != nil && *process.Status.ExitCode != 0 {
		return &ProcessExitError{Process: process.Name, Code: *process.Status.ExitCode}
	}
	return &ProcessTerminationError{Process: process.Name, Phase: process.Status.Phase, Reason: process.Status.TerminationReason}
}

func (processes *ProcessClient) Start(ctx context.Context, request ProcessStartRequest) (*workspacesv1alpha1.AgentProcess, error) {
	if len(request.Command) == 0 {
		return nil, fmt.Errorf("command is required")
	}
	prefix := request.NamePrefix
	if prefix == "" {
		prefix = AgentTypeForCommand(request.Command[0])
		if prefix == "" {
			prefix = "process"
		}
	}
	nameGenerator := processes.NameGenerator
	if nameGenerator == nil {
		nameGenerator = GenerateSortableName
	}
	name := nameGenerator(prefix)
	envSecretName := ""
	variables := make([]workspacesv1alpha1.ProcessEnvironmentVariable, 0, len(request.Environment))
	if len(request.Environment) > 0 {
		envSecretName = boundedDNSName(name + "-env")
		names := make([]string, 0, len(request.Environment))
		for variable := range request.Environment {
			names = append(names, variable)
		}
		slices.Sort(names)
		for _, variable := range names {
			variables = append(variables, workspacesv1alpha1.ProcessEnvironmentVariable{Name: variable, Key: variable})
		}
	}
	process := &workspacesv1alpha1.AgentProcess{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: request.Namespace},
		Spec: workspacesv1alpha1.AgentProcessSpec{
			TargetRef: request.Target, Command: append([]string(nil), request.Command...),
			WorkingDirectory: request.WorkingDirectory, TTY: request.TTY,
			DesiredState: workspacesv1alpha1.AgentProcessDesiredStateRunning,
			AgentType:    request.AgentType, Env: variables,
		},
	}
	if envSecretName != "" {
		process.Spec.EnvSecretRef = &workspacesv1alpha1.LocalReference{Name: envSecretName}
	}
	if request.AgentCredential != "" {
		process.Spec.AgentCredentialRef = &workspacesv1alpha1.LocalReference{Name: request.AgentCredential}
	}
	for _, credential := range request.Credentials {
		process.Spec.CredentialRefs = append(process.Spec.CredentialRefs, workspacesv1alpha1.LocalReference{Name: credential})
	}
	owner, err := processes.processOwner(ctx, request.Namespace, request.Target)
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(owner, process, processes.Kube.Scheme()); err != nil {
		return nil, fmt.Errorf("set process target owner on AgentProcess: %w", err)
	}
	if err := processes.Kube.Create(ctx, process); err != nil {
		return nil, fmt.Errorf("create AgentProcess: %w", err)
	}
	if envSecretName != "" {
		data := make(map[string][]byte, len(request.Environment))
		for variable, value := range request.Environment {
			data[variable] = []byte(value)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: envSecretName, Namespace: request.Namespace},
			Immutable:  boolValuePointer(true),
			Data:       data,
		}
		if err := controllerutil.SetControllerReference(process, secret, processes.Kube.Scheme()); err != nil {
			return nil, fmt.Errorf("set AgentProcess owner on environment Secret: %w", err)
		}
		if err := processes.Kube.Create(ctx, secret); err != nil {
			return nil, fmt.Errorf("create AgentProcess environment Secret: %w", err)
		}
	}

	return process, nil
}

func (processes *ProcessClient) processOwner(ctx context.Context, namespace string, target workspacesv1alpha1.AgentProcessTargetReference) (client.Object, error) {
	key := client.ObjectKey{Name: target.Name, Namespace: namespace}
	switch target.Kind {
	case workspacesv1alpha1.AgentProcessTargetWorkspace:
		workspace := new(workspacesv1alpha1.Workspace)
		if err := processes.Kube.Get(ctx, key, workspace); err != nil {
			return nil, fmt.Errorf("get AgentProcess Workspace owner: %w", err)
		}
		return workspace, nil
	case workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment:
		environment := new(workspacesv1alpha1.WorkspaceEnvironment)
		if err := processes.Kube.Get(ctx, key, environment); err != nil {
			return nil, fmt.Errorf("get AgentProcess WorkspaceEnvironment owner: %w", err)
		}
		return environment, nil
	default:
		return nil, fmt.Errorf("unsupported AgentProcess target kind %s", target.Kind)
	}
}

func (processes *ProcessClient) WaitUntilAttachable(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (*workspacesv1alpha1.AgentProcess, error) {
	return processes.wait(ctx, process, func(current *workspacesv1alpha1.AgentProcess) (bool, error) {
		if current.Status.Phase == workspacesv1alpha1.AgentProcessPhaseRunning {
			return true, nil
		}
		if processPhaseTerminal(current.Status.Phase) {
			return true, nil
		}
		return false, nil
	})
}

func (processes *ProcessClient) WaitUntilTerminal(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (*workspacesv1alpha1.AgentProcess, error) {
	return processes.wait(ctx, process, func(current *workspacesv1alpha1.AgentProcess) (bool, error) {
		return processPhaseTerminal(current.Status.Phase), nil
	})
}

func (processes *ProcessClient) wait(ctx context.Context, process *workspacesv1alpha1.AgentProcess, done func(*workspacesv1alpha1.AgentProcess) (bool, error)) (*workspacesv1alpha1.AgentProcess, error) {
	var result *workspacesv1alpha1.AgentProcess
	err := wait.PollUntilContextCancel(ctx, 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(workspacesv1alpha1.AgentProcess)
		if err := processes.Kube.Get(ctx, client.ObjectKeyFromObject(process), current); err != nil {
			return false, fmt.Errorf("get AgentProcess: %w", err)
		}
		complete, err := done(current)
		if complete {
			result = current
		}
		return complete, err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (processes *ProcessClient) Resolve(ctx context.Context, namespace string, idOrPrefix string) (*workspacesv1alpha1.AgentProcess, error) {
	exact := new(workspacesv1alpha1.AgentProcess)
	if err := processes.Kube.Get(ctx, types.NamespacedName{Name: idOrPrefix, Namespace: namespace}, exact); err == nil {
		return exact, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get AgentProcess %q: %w", idOrPrefix, err)
	}
	list := new(workspacesv1alpha1.AgentProcessList)
	if err := processes.Kube.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list AgentProcesses: %w", err)
	}
	matches := make([]*workspacesv1alpha1.AgentProcess, 0, 1)
	for index := range list.Items {
		if strings.HasPrefix(list.Items[index].Name, idOrPrefix) {
			matches = append(matches, &list.Items[index])
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("agent process %q was not found in namespace %q", idOrPrefix, namespace)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("agent process prefix %q is ambiguous", idOrPrefix)
	}

	return matches[0], nil
}

func (processes *ProcessClient) Attach(ctx context.Context, process *workspacesv1alpha1.AgentProcess, input io.Reader, output io.Writer) error {
	if process.Status.RuntimePodName == "" {
		return fmt.Errorf("agent process %s has no runtime Pod", process.Name)
	}
	target := processruntime.Target{Namespace: process.Namespace, Pod: process.Status.RuntimePodName, Container: "rc-kube"}
	clientID := GenerateSortableName("terminal")
	rows, columns, cleanup, err := processes.prepareLocalTerminal(ctx, target, process, clientID, input)
	if err != nil {
		return err
	}
	defer cleanup()

	return processes.Runtime.Attach(ctx, target, process.Name, clientID, input, output, process.Spec.TTY, rows, columns)
}

func (processes *ProcessClient) prepareLocalTerminal(ctx context.Context, target processruntime.Target, process *workspacesv1alpha1.AgentProcess, clientID string, input io.Reader) (uint16, uint16, func(), error) {
	inputFile, ok := input.(*os.File)
	if !process.Spec.TTY || !ok || !term.IsTerminal(int(inputFile.Fd())) {
		return 0, 0, func() {}, nil
	}
	columns, rows, err := term.GetSize(int(inputFile.Fd()))
	if err != nil {
		return 0, 0, nil, fmt.Errorf("read local terminal size: %w", err)
	}
	state, err := term.MakeRaw(int(inputFile.Fd()))
	if err != nil {
		return 0, 0, nil, fmt.Errorf("put local terminal in raw mode: %w", err)
	}
	resize := func() {
		currentColumns, currentRows, sizeErr := term.GetSize(int(inputFile.Fd()))
		if sizeErr == nil && currentColumns > 0 && currentRows > 0 {
			_ = processes.Runtime.Resize(ctx, target, process.Name, clientID, uint16(currentRows), uint16(currentColumns))
		}
	}
	resizeSignals := make(chan os.Signal, 1)
	stopResize := make(chan struct{})
	resizeDone := make(chan struct{})
	stopResizeNotifications := subscribeTerminalResize(resizeSignals)
	go func() {
		defer close(resizeDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopResize:
				return
			case <-resizeSignals:
				resize()
			}
		}
	}()

	return uint16(rows), uint16(columns), func() {
		stopResizeNotifications()
		close(stopResize)
		<-resizeDone
		_ = term.Restore(int(inputFile.Fd()), state)
	}, nil
}

func (processes *ProcessClient) Stop(ctx context.Context, process *workspacesv1alpha1.AgentProcess) error {
	current := new(workspacesv1alpha1.AgentProcess)
	if err := processes.Kube.Get(ctx, client.ObjectKeyFromObject(process), current); err != nil {
		return fmt.Errorf("re-fetch AgentProcess before stop: %w", err)
	}
	current.Spec.DesiredState = workspacesv1alpha1.AgentProcessDesiredStateStopped
	if err := processes.Kube.Update(ctx, current); err != nil {
		return fmt.Errorf("request AgentProcess stop: %w", err)
	}

	return nil
}

func (processes *ProcessClient) Logs(ctx context.Context, process *workspacesv1alpha1.AgentProcess, output io.Writer) error {
	if process.Status.RuntimePodName != "" {
		pod := new(corev1.Pod)
		key := types.NamespacedName{Name: process.Status.RuntimePodName, Namespace: process.Namespace}
		if err := processes.Kube.Get(ctx, key, pod); err == nil && string(pod.UID) == process.Status.RuntimePodUID {
			target := processruntime.Target{Namespace: process.Namespace, Pod: pod.Name, Container: "rc-kube"}
			if err := processes.Runtime.Logs(ctx, target, process.Name, output); err == nil {
				return nil
			}
		}
	}

	claimName, image, err := processes.logVolume(ctx, process)
	if err != nil {
		return err
	}
	if processes.Config == nil {
		return fmt.Errorf("kubernetes REST config is required for suspended transcript reads")
	}
	helperName := boundedDNSName(process.Name + "-logs")
	automount := false
	helper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: helperName, Namespace: process.Namespace},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &automount, RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "reader", Image: image, Command: []string{"sh", "-c", "cat \"$1\"", "rc-log-reader", "/home/agent/" + process.Status.TranscriptPath},
				VolumeMounts: []corev1.VolumeMount{{Name: "home", MountPath: WorkspaceHomePath, ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName, ReadOnly: true}}}},
		},
	}
	if err := controllerutil.SetControllerReference(process, helper, processes.Kube.Scheme()); err != nil {
		return fmt.Errorf("set AgentProcess owner on log helper Pod: %w", err)
	}
	if err := processes.Kube.Create(ctx, helper); err != nil {
		return fmt.Errorf("create AgentProcess log helper Pod: %w", err)
	}
	defer func() { _ = processes.Kube.Delete(context.Background(), helper) }()
	if err := wait.PollUntilContextCancel(ctx, 300*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		current := new(corev1.Pod)
		if err := processes.Kube.Get(ctx, client.ObjectKeyFromObject(helper), current); err != nil {
			return false, err
		}
		return current.Status.Phase == corev1.PodRunning || current.Status.Phase == corev1.PodSucceeded || current.Status.Phase == corev1.PodFailed, nil
	}); err != nil {
		return fmt.Errorf("wait for AgentProcess log helper Pod: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(processes.Config)
	if err != nil {
		return fmt.Errorf("create Kubernetes clientset for logs: %w", err)
	}
	stream, err := clientset.CoreV1().Pods(process.Namespace).GetLogs(helper.Name, &corev1.PodLogOptions{Container: "reader"}).Stream(ctx)
	if err != nil {
		return fmt.Errorf("stream AgentProcess log helper output: %w", err)
	}
	defer func() { _ = stream.Close() }()
	_, err = io.Copy(output, stream)

	return err
}

func (processes *ProcessClient) logVolume(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (string, string, error) {
	switch process.Spec.TargetRef.Kind {
	case workspacesv1alpha1.AgentProcessTargetWorkspace:
		workspace := new(workspacesv1alpha1.Workspace)
		key := types.NamespacedName{Name: process.Spec.TargetRef.Name, Namespace: process.Namespace}
		if err := processes.Kube.Get(ctx, key, workspace); err != nil {
			return "", "", fmt.Errorf("get AgentProcess Workspace for logs: %w", err)
		}
		return workspace.Status.HomeVolumeClaimName, workspace.Status.RuntimeImage, nil
	case workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment:
		environment := new(workspacesv1alpha1.WorkspaceEnvironment)
		key := types.NamespacedName{Name: process.Spec.TargetRef.Name, Namespace: process.Namespace}
		if err := processes.Kube.Get(ctx, key, environment); err != nil {
			return "", "", fmt.Errorf("get AgentProcess WorkspaceEnvironment for logs: %w", err)
		}
		claimName := environment.Status.DraftVolumeClaimName
		if claimName == "" {
			claimName = environment.Status.CurrentVolumeClaimName
		}
		return claimName, environment.Spec.Image, nil
	default:
		return "", "", fmt.Errorf("agent process %s has unsupported target kind %s", process.Name, process.Spec.TargetRef.Kind)
	}
}

func processPhaseTerminal(phase workspacesv1alpha1.AgentProcessPhase) bool {
	switch phase {
	case workspacesv1alpha1.AgentProcessPhaseSucceeded, workspacesv1alpha1.AgentProcessPhaseFailed,
		workspacesv1alpha1.AgentProcessPhaseStopped, workspacesv1alpha1.AgentProcessPhaseLost:
		return true
	default:
		return false
	}
}

func AgentTypeForCommand(command string) string {
	name := strings.ToLower(strings.TrimSpace(command))
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	switch name {
	case "codex", "claude", "opencode", "gemini", "orca":
		return name
	default:
		return ""
	}
}

func boolValuePointer(value bool) *bool {
	return &value
}
