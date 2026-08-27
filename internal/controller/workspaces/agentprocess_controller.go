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
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const (
	processTargetRequeueDelay = 2 * time.Second
	agentProcessFinalizer     = "workspaces.rc.ayaka.io/agent-process"
	workspaceSSHConfigPath    = workspaceHomeMountPath + "/.ssh/config"
)

type resolvedProcessTarget struct {
	runtime               processruntime.Target
	podUID                string
	workingDir            string
	environment           map[string]string
	agentHome             string
	credentials           map[string][]byte
	mounts                []processruntime.CredentialMount
	credentialEnvironment map[string]string
	sshConfigFragments    map[string]string
}

type resolvedCredentialProjection struct {
	mounts             []processruntime.CredentialMount
	environment        map[string]string
	sshConfigFragments map[string]string
}

// AgentProcessReconciler reconciles an at-most-once command with rc-kube.
type AgentProcessReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Runtime processruntime.Runtime
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=agentprocesses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=agentprocesses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=agentprocesses/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces;workspaceenvironments,verbs=get;list;watch
// +kubebuilder:rbac:groups=configs.rc.ayaka.io,resources=agentcredentials;credentials,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods;secrets;configmaps;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch

func (r *AgentProcessReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	process := new(workspacesv1alpha1.AgentProcess)
	if err := r.Get(ctx, req.NamespacedName, process); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if result, handled, err := r.handleAgentProcessLifecycle(ctx, process); handled {
		return result, err
	}
	if r.Runtime == nil {
		return ctrl.Result{}, errors.New("AgentProcess runtime client is not configured")
	}

	desiredState := process.Spec.DesiredState
	if desiredState == "" {
		desiredState = workspacesv1alpha1.AgentProcessDesiredStateRunning
	}
	if desiredState == workspacesv1alpha1.AgentProcessDesiredStateStopped {
		if process.Status.Phase == "" || process.Status.Phase == workspacesv1alpha1.AgentProcessPhasePending {
			return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseStopped, nil, "StoppedBeforeStart", "Process was stopped before it started", 0)
		}
		target, lost, err := r.originalProcessTarget(ctx, process)
		if err != nil {
			return ctrl.Result{}, err
		}
		if lost {
			return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseLost, nil, "RuntimeReplaced", "The original runtime Pod no longer exists", 0)
		}
		state, err := r.Runtime.Stop(ctx, target.runtime, process.Name)
		if err != nil {
			if errors.Is(err, processruntime.ErrNotFound) {
				return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseLost, nil, "ProcessMissing", "rc-kube no longer owns the original process", 0)
			}
			return ctrl.Result{}, fmt.Errorf("stop Agent Process through rc-kube: %w", err)
		}

		return ctrl.Result{}, r.applyRuntimeState(ctx, req.NamespacedName, target, state)
	}

	if process.Status.Phase == workspacesv1alpha1.AgentProcessPhaseRunning {
		target, lost, err := r.originalProcessTarget(ctx, process)
		if err != nil {
			return ctrl.Result{}, err
		}
		if lost {
			return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseLost, nil, "RuntimeReplaced", "The original runtime Pod no longer exists", 0)
		}
		state, err := r.Runtime.Inspect(ctx, target.runtime, process.Name)
		if err != nil {
			if errors.Is(err, processruntime.ErrNotFound) {
				return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseLost, nil, "ProcessMissing", "rc-kube no longer owns the original process", 0)
			}
			return ctrl.Result{}, fmt.Errorf("inspect Agent Process through rc-kube: %w", err)
		}

		return ctrl.Result{RequeueAfter: processTargetRequeueDelay}, r.applyRuntimeState(ctx, req.NamespacedName, target, state)
	}

	target, reason, message, err := r.resolveProcessTarget(ctx, process)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		if err := r.setProcessCondition(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhasePending, metav1.ConditionFalse, reason, message); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: processTargetRequeueDelay}, nil
	}
	if process.Status.RuntimePodUID != "" && process.Status.RuntimePodUID != target.podUID {
		return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseLost, nil, "RuntimeReplaced", "The original runtime Pod no longer exists", 0)
	}

	if err := r.claimProcessRuntime(ctx, req.NamespacedName, target); err != nil {
		return ctrl.Result{}, err
	}
	request, err := r.processStartRequest(ctx, process, target)
	if err != nil {
		return ctrl.Result{}, err
	}
	current := new(workspacesv1alpha1.AgentProcess)
	if err := r.Get(ctx, req.NamespacedName, current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !current.DeletionTimestamp.IsZero() {
		return ctrl.Result{Requeue: true}, nil
	}
	state, err := r.Runtime.Start(ctx, target.runtime, request)
	if err != nil {
		if errors.Is(err, processruntime.ErrNotFound) {
			return ctrl.Result{}, r.setTerminalProcessStatus(ctx, req.NamespacedName, workspacesv1alpha1.AgentProcessPhaseLost, nil, "ProcessOwnershipLost", "rc-kube retained the process identity but no longer owns the original process", 0)
		}
		return ctrl.Result{}, fmt.Errorf("start Agent Process through rc-kube: %w", err)
	}
	log.Info("Started Agent Process", "name", process.Name, "runtimePod", target.runtime.Pod)

	return ctrl.Result{RequeueAfter: processTargetRequeueDelay}, r.applyRuntimeState(ctx, req.NamespacedName, target, state)
}

func (r *AgentProcessReconciler) handleAgentProcessLifecycle(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (ctrl.Result, bool, error) {
	if !process.DeletionTimestamp.IsZero() {
		result, err := r.finalizeAgentProcess(ctx, process)
		return result, true, err
	}
	if agentProcessTerminal(process.Status.Phase) {
		if controllerutil.ContainsFinalizer(process, agentProcessFinalizer) {
			controllerutil.RemoveFinalizer(process, agentProcessFinalizer)
			if err := r.Update(ctx, process); err != nil {
				return ctrl.Result{}, true, fmt.Errorf("remove terminal AgentProcess finalizer: %w", err)
			}
		}
		return ctrl.Result{}, true, nil
	}
	if controllerutil.ContainsFinalizer(process, agentProcessFinalizer) {
		return ctrl.Result{}, false, nil
	}
	controllerutil.AddFinalizer(process, agentProcessFinalizer)
	if err := r.Update(ctx, process); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("add AgentProcess finalizer: %w", err)
	}

	return ctrl.Result{Requeue: true}, true, nil
}

func (r *AgentProcessReconciler) finalizeAgentProcess(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(process, agentProcessFinalizer) {
		return ctrl.Result{}, nil
	}
	if process.Status.RuntimePodName != "" && process.Status.RuntimePodUID != "" {
		if r.Runtime == nil {
			return ctrl.Result{}, errors.New("AgentProcess runtime client is not configured")
		}
		target, lost, err := r.originalProcessTarget(ctx, process)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !lost {
			state, err := r.Runtime.Stop(ctx, target.runtime, process.Name)
			if err != nil && !errors.Is(err, processruntime.ErrNotFound) {
				return ctrl.Result{}, fmt.Errorf("stop deleting Agent Process through rc-kube: %w", err)
			}
			if err == nil && !agentProcessTerminal(runtimePhase(state.Phase, state.ExitCode)) {
				return ctrl.Result{RequeueAfter: processTargetRequeueDelay}, nil
			}
		}
	}
	controllerutil.RemoveFinalizer(process, agentProcessFinalizer)
	if err := r.Update(ctx, process); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove AgentProcess finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *AgentProcessReconciler) originalProcessTarget(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (*resolvedProcessTarget, bool, error) {
	if process.Status.RuntimePodName == "" || process.Status.RuntimePodUID == "" {
		return nil, true, nil
	}
	pod := new(corev1.Pod)
	key := types.NamespacedName{Name: process.Status.RuntimePodName, Namespace: process.Namespace}
	if err := r.Get(ctx, key, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("get original Agent Process runtime Pod: %w", err)
	}
	if string(pod.UID) != process.Status.RuntimePodUID {
		return nil, true, nil
	}

	return &resolvedProcessTarget{
		runtime: processruntime.Target{Namespace: process.Namespace, Pod: pod.Name, Container: runtimeContainerName},
		podUID:  string(pod.UID),
	}, false, nil
}

func (r *AgentProcessReconciler) resolveProcessTarget(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (*resolvedProcessTarget, string, string, error) {
	switch process.Spec.TargetRef.Kind {
	case workspacesv1alpha1.AgentProcessTargetWorkspace:
		workspace := new(workspacesv1alpha1.Workspace)
		key := types.NamespacedName{Name: process.Spec.TargetRef.Name, Namespace: process.Namespace}
		if err := r.Get(ctx, key, workspace); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "TargetNotFound", "Target Workspace does not exist", nil
			}
			return nil, "", "", fmt.Errorf("get target Workspace: %w", err)
		}
		if !meta.IsStatusConditionTrue(workspace.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady) || workspace.Status.RuntimePodName == "" {
			return nil, reasonTargetNotReady, "Target Workspace is not ready", nil
		}
		pod := new(corev1.Pod)
		podKey := types.NamespacedName{Name: workspace.Status.RuntimePodName, Namespace: workspace.Namespace}
		if err := r.Get(ctx, podKey, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, reasonTargetNotReady, "Target Workspace runtime Pod does not exist", nil
			}
			return nil, "", "", fmt.Errorf("get target Workspace runtime Pod: %w", err)
		}
		workingDirectory := process.Spec.WorkingDirectory
		if workingDirectory == "" {
			workingDirectory = workspace.Spec.DefaultWorkingDirectory
		}
		if workingDirectory == "" {
			workingDirectory = workspaceRootMountPath
		}
		environment, err := r.resolveWorkspaceEnvironmentVariables(ctx, workspace, process)
		if err != nil {
			return nil, "", "", err
		}
		agentHome, credentialFiles, err := r.resolveProcessCredentials(ctx, workspace, process)
		if err != nil {
			return nil, "", "", err
		}
		credentialProjection, err := r.resolveCredentialProjections(ctx, process.Namespace, process.Spec.CredentialRefs)
		if err != nil {
			return nil, "", "", err
		}

		return &resolvedProcessTarget{
			runtime: processruntime.Target{Namespace: process.Namespace, Pod: pod.Name, Container: runtimeContainerName},
			podUID:  string(pod.UID), workingDir: workingDirectory, environment: environment,
			agentHome: agentHome, credentials: credentialFiles, mounts: credentialProjection.mounts, credentialEnvironment: credentialProjection.environment,
			sshConfigFragments: credentialProjection.sshConfigFragments,
		}, "", "", nil
	case workspacesv1alpha1.AgentProcessTargetWorkspaceEnvironment:
		return r.resolveEnvironmentProcessTarget(ctx, process)
	default:
		return nil, "InvalidTarget", "Agent Process target kind is not supported", nil
	}
}

func (r *AgentProcessReconciler) resolveWorkspaceEnvironmentVariables(ctx context.Context, workspace *workspacesv1alpha1.Workspace, process *workspacesv1alpha1.AgentProcess) (map[string]string, error) {
	values := make(map[string]string, len(workspace.Spec.Env)+len(process.Spec.Env)+2)
	for _, variable := range workspace.Spec.Env {
		value, err := r.resolveEnvVar(ctx, workspace.Namespace, variable)
		if err != nil {
			return nil, fmt.Errorf("resolve Workspace environment variable %s: %w", variable.Name, err)
		}
		values[variable.Name] = value
	}
	if process.Spec.EnvSecretRef != nil {
		secret := new(corev1.Secret)
		key := types.NamespacedName{Name: process.Spec.EnvSecretRef.Name, Namespace: process.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			return nil, fmt.Errorf("get Agent Process environment Secret: %w", err)
		}
		for _, variable := range process.Spec.Env {
			secretKey := variable.Key
			if secretKey == "" {
				secretKey = variable.Name
			}
			value, ok := secret.Data[secretKey]
			if !ok {
				return nil, fmt.Errorf("agent process environment Secret %s has no key %s", secret.Name, secretKey)
			}
			values[variable.Name] = string(value)
		}
	}

	return values, nil
}

func (r *AgentProcessReconciler) resolveEnvVar(ctx context.Context, namespace string, variable corev1.EnvVar) (string, error) {
	if variable.ValueFrom == nil {
		return variable.Value, nil
	}
	if variable.ValueFrom.SecretKeyRef != nil {
		selector := variable.ValueFrom.SecretKeyRef
		secret := new(corev1.Secret)
		if err := r.Get(ctx, types.NamespacedName{Name: selector.Name, Namespace: namespace}, secret); err != nil {
			return "", err
		}
		value, ok := secret.Data[selector.Key]
		if !ok {
			return "", fmt.Errorf("secret %s has no key %s", selector.Name, selector.Key)
		}

		return string(value), nil
	}
	if variable.ValueFrom.ConfigMapKeyRef != nil {
		selector := variable.ValueFrom.ConfigMapKeyRef
		configMap := new(corev1.ConfigMap)
		if err := r.Get(ctx, types.NamespacedName{Name: selector.Name, Namespace: namespace}, configMap); err != nil {
			return "", err
		}
		value, ok := configMap.Data[selector.Key]
		if !ok {
			return "", fmt.Errorf("config map %s has no key %s", selector.Name, selector.Key)
		}

		return value, nil
	}

	return "", errors.New("fieldRef and resourceFieldRef are not supported for Agent Process environment")
}

func (r *AgentProcessReconciler) resolveProcessCredentials(ctx context.Context, workspace *workspacesv1alpha1.Workspace, process *workspacesv1alpha1.AgentProcess) (string, map[string][]byte, error) {
	credentialFiles := make(map[string][]byte)
	agentCredentialRef := process.Spec.AgentCredentialRef
	if agentCredentialRef == nil && process.Spec.AgentType != "" && len(workspace.Spec.AgentCredentialRefs) > 0 {
		for index := range workspace.Spec.AgentCredentialRefs {
			reference := &workspace.Spec.AgentCredentialRefs[index]
			credential := new(configsv1alpha1.AgentCredential)
			key := types.NamespacedName{Name: reference.Name, Namespace: workspace.Namespace}
			if err := r.Get(ctx, key, credential); err != nil {
				return "", nil, fmt.Errorf("get AgentCredential: %w", err)
			}
			if string(credential.Spec.Agent) == process.Spec.AgentType {
				agentCredentialRef = reference
				break
			}
		}
	}
	agentHome := ""
	if process.Spec.AgentType != "" {
		credentialName := defaultCredentialName
		if agentCredentialRef != nil {
			credentialName = agentCredentialRef.Name
		}
		agentHome = filepath.Join(workspaceHomeMountPath, ".rc", "agents", process.Spec.AgentType, credentialName)
	}
	if agentCredentialRef != nil {
		if !containsReference(workspace.Spec.AgentCredentialRefs, agentCredentialRef.Name) {
			return "", nil, fmt.Errorf("agent credential %s is not referenced by Workspace %s", agentCredentialRef.Name, workspace.Name)
		}
		agentCredential := new(configsv1alpha1.AgentCredential)
		key := types.NamespacedName{Name: agentCredentialRef.Name, Namespace: workspace.Namespace}
		if err := r.Get(ctx, key, agentCredential); err != nil {
			return "", nil, fmt.Errorf("get AgentCredential: %w", err)
		}
		if process.Spec.AgentType != "" && string(agentCredential.Spec.Agent) != process.Spec.AgentType {
			return "", nil, fmt.Errorf("agent credential %s has agent type %s, not %s", agentCredential.Name, agentCredential.Spec.Agent, process.Spec.AgentType)
		}
		secret := new(corev1.Secret)
		secretKey := types.NamespacedName{Name: agentCredential.Spec.SecretKeyRef.Name, Namespace: workspace.Namespace}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return "", nil, fmt.Errorf("get AgentCredential Secret: %w", err)
		}
		data, ok := secret.Data[agentCredential.Spec.SecretKeyRef.Key]
		if !ok {
			return "", nil, fmt.Errorf("agent credential Secret %s has no key %s", secret.Name, agentCredential.Spec.SecretKeyRef.Key)
		}
		credentialFiles["agent/auth.json"] = append([]byte(nil), data...)
	}
	for _, reference := range process.Spec.CredentialRefs {
		if !containsReference(workspace.Spec.CredentialRefs, reference.Name) {
			return "", nil, fmt.Errorf("credential %s is not referenced by Workspace %s", reference.Name, workspace.Name)
		}
		files, err := r.resolveGenericCredential(ctx, workspace.Namespace, reference.Name)
		if err != nil {
			return "", nil, err
		}
		for name, data := range files {
			credentialFiles[filepath.Join("credentials", reference.Name, name)] = data
		}
	}

	return agentHome, credentialFiles, nil
}

func containsReference(references []workspacesv1alpha1.LocalReference, name string) bool {
	for _, reference := range references {
		if reference.Name == name {
			return true
		}
	}

	return false
}

func (r *AgentProcessReconciler) resolveGenericCredential(ctx context.Context, namespace string, name string) (map[string][]byte, error) {
	credential := new(configsv1alpha1.Credential)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, credential); err != nil {
		return nil, fmt.Errorf("get Credential %s: %w", name, err)
	}
	files := make(map[string][]byte)
	read := func(reference configsv1alpha1.SecretKeyReference) ([]byte, error) {
		secret := new(corev1.Secret)
		if err := r.Get(ctx, types.NamespacedName{Name: reference.Name, Namespace: namespace}, secret); err != nil {
			return nil, err
		}
		data, ok := secret.Data[reference.Key]
		if !ok {
			return nil, fmt.Errorf("secret %s has no key %s", secret.Name, reference.Key)
		}

		return append([]byte(nil), data...), nil
	}
	var err error
	switch credential.Spec.Type {
	case configsv1alpha1.CredentialTypeSSHPrivateKey:
		files["id"], err = read(credential.Spec.SSHPrivateKey.PrivateKeyRef)
		if err == nil {
			files["known_hosts"], err = read(credential.Spec.SSHPrivateKey.KnownHostsRef)
		}
	case configsv1alpha1.CredentialTypeHTTPBasicAuth:
		files["username"] = []byte(credential.Spec.HTTPBasicAuth.Username)
		files["password"], err = read(credential.Spec.HTTPBasicAuth.PasswordRef)
	case configsv1alpha1.CredentialTypeHTTPBearerToken:
		files["token"], err = read(credential.Spec.HTTPBearerToken.TokenRef)
	case configsv1alpha1.CredentialTypeHTTPHeaders:
		for _, header := range credential.Spec.HTTPHeaders.Headers {
			files[header.Name], err = read(header.ValueRef)
			if err != nil {
				break
			}
		}
	case configsv1alpha1.CredentialTypeProcess:
		if credential.Spec.Process == nil {
			return nil, fmt.Errorf("process Credential %s has no process configuration", credential.Name)
		}
		for index, file := range credential.Spec.Process.Files {
			files[filepath.Join("files", strconv.Itoa(index))], err = read(file.DataRef)
			if err != nil {
				break
			}
		}
	default:
		return nil, fmt.Errorf("credential %s has unsupported type %s", credential.Name, credential.Spec.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("read Credential %s secret data: %w", credential.Name, err)
	}

	return files, nil
}

func (r *AgentProcessReconciler) resolveCredentialProjections(ctx context.Context, namespace string, references []workspacesv1alpha1.LocalReference) (resolvedCredentialProjection, error) {
	projection := resolvedCredentialProjection{
		mounts: make([]processruntime.CredentialMount, 0), environment: make(map[string]string), sshConfigFragments: make(map[string]string),
	}
	targets := make(map[string]string)
	for _, reference := range references {
		credential := new(configsv1alpha1.Credential)
		if err := r.Get(ctx, types.NamespacedName{Name: reference.Name, Namespace: namespace}, credential); err != nil {
			return resolvedCredentialProjection{}, fmt.Errorf("get Credential %s projection: %w", reference.Name, err)
		}
		if credential.Spec.Type == configsv1alpha1.CredentialTypeSSHPrivateKey && credential.Spec.SSHPrivateKey != nil && credential.Spec.SSHPrivateKey.Config != "" {
			projection.sshConfigFragments[reference.Name] = credential.Spec.SSHPrivateKey.Config
		}
		if credential.Spec.Type != configsv1alpha1.CredentialTypeProcess {
			continue
		}
		if credential.Spec.Process == nil {
			return resolvedCredentialProjection{}, fmt.Errorf("process Credential %s has no process configuration", reference.Name)
		}
		for index, file := range credential.Spec.Process.Files {
			mountPath := file.MountPath
			if owner, exists := targets[mountPath]; exists {
				return resolvedCredentialProjection{}, fmt.Errorf("credentials %s and %s project to the same path %s", owner, reference.Name, mountPath)
			}
			targets[mountPath] = reference.Name
			projection.mounts = append(projection.mounts, processruntime.CredentialMount{
				Source: filepath.Join("credentials", reference.Name, "files", strconv.Itoa(index)), Target: mountPath,
			})
		}
		for _, variable := range credential.Spec.Process.Envs {
			if previous, exists := projection.environment[variable.Name]; exists && previous != variable.Value {
				return resolvedCredentialProjection{}, fmt.Errorf("selected Credentials define conflicting values for environment variable %s", variable.Name)
			}
			projection.environment[variable.Name] = variable.Value
		}
	}
	return projection, nil
}

func (r *AgentProcessReconciler) processStartRequest(ctx context.Context, process *workspacesv1alpha1.AgentProcess, target *resolvedProcessTarget) (processruntime.StartRequest, error) {
	environment := make(map[string]string, len(target.environment)+3)
	maps.Copy(environment, target.credentialEnvironment)
	maps.Copy(environment, target.environment)
	if target.agentHome != "" {
		if _, exists := environment["RC_AGENT_HOME"]; !exists {
			environment["RC_AGENT_HOME"] = target.agentHome
		} else {
			logf.FromContext(ctx).Info("AgentProcess environment overrides automatic Agent home", "name", process.Name, "variable", "RC_AGENT_HOME")
		}
		if process.Spec.AgentType == agentTypeCodex {
			if _, exists := environment["CODEX_HOME"]; !exists {
				environment["CODEX_HOME"] = target.agentHome
			} else {
				logf.FromContext(ctx).Info("AgentProcess environment overrides automatic Agent home", "name", process.Name, "variable", "CODEX_HOME")
			}
		}
	}
	if len(process.Spec.CredentialRefs) > 0 {
		environment["RC_CREDENTIALS_DIR"] = "/run/rc/credentials"
	}
	select {
	case <-ctx.Done():
		return processruntime.StartRequest{}, ctx.Err()
	default:
	}

	return processruntime.StartRequest{
		ID: process.Name, UID: string(process.UID), Command: append([]string(nil), process.Spec.Command...),
		WorkingDirectory: target.workingDir, TTY: process.Spec.TTY, Environment: environment,
		AgentHome: target.agentHome, CredentialFiles: target.credentials, CredentialMounts: append([]processruntime.CredentialMount(nil), target.mounts...),
		RuntimeDirectory: filepath.Join("/run/rc/processes", process.Name), CredentialsRoot: "/run/rc/credentials",
		TranscriptPath: filepath.Join(workspaceHomeMountPath, ".rc", "processes", process.Name, "transcript.log"),
		SSHConfigPath:  sshConfigPath(target.sshConfigFragments), SSHConfigFragments: maps.Clone(target.sshConfigFragments),
	}, nil
}

func sshConfigPath(fragments map[string]string) string {
	if len(fragments) == 0 {
		return ""
	}
	return workspaceSSHConfigPath
}

func (r *AgentProcessReconciler) claimProcessRuntime(ctx context.Context, key types.NamespacedName, target *resolvedProcessTarget) error {
	current := new(workspacesv1alpha1.AgentProcess)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch AgentProcess before runtime claim: %w", err)
	}
	current.Status.ObservedGeneration = current.Generation
	current.Status.Phase = workspacesv1alpha1.AgentProcessPhaseStarting
	current.Status.RuntimePodName = target.runtime.Pod
	current.Status.RuntimePodUID = target.podUID
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.AgentProcessConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: current.Generation, Reason: "Starting", Message: "rc-kube is starting the command",
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("persist AgentProcess runtime claim: %w", err)
	}

	return nil
}

func (r *AgentProcessReconciler) applyRuntimeState(ctx context.Context, key types.NamespacedName, target *resolvedProcessTarget, state processruntime.State) error {
	phase := runtimePhase(state.Phase, state.ExitCode)
	if agentProcessTerminal(phase) {
		return r.setTerminalProcessStatus(ctx, key, phase, state.ExitCode, state.Reason, runtimeMessage(phase), state.AttachedClients)
	}
	current := new(workspacesv1alpha1.AgentProcess)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch AgentProcess before status update: %w", err)
	}
	now := metav1.Now()
	current.Status.ObservedGeneration = current.Generation
	current.Status.Phase = phase
	current.Status.RuntimePodName = target.runtime.Pod
	current.Status.RuntimePodUID = target.podUID
	if current.Status.StartedAt == nil && phase == workspacesv1alpha1.AgentProcessPhaseRunning {
		current.Status.StartedAt = &now
	}
	current.Status.AttachedClients = state.AttachedClients
	current.Status.TranscriptPath = filepath.Join(".rc", "processes", current.Name, "transcript.log")
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.AgentProcessConditionReady, Status: metav1.ConditionTrue,
		ObservedGeneration: current.Generation, Reason: "ProcessRunning", Message: "rc-kube owns the running process",
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update running AgentProcess status: %w", err)
	}

	return nil
}

func runtimePhase(phase string, exitCode *int32) workspacesv1alpha1.AgentProcessPhase {
	switch strings.ToLower(phase) {
	case "running":
		return workspacesv1alpha1.AgentProcessPhaseRunning
	case "stopped":
		return workspacesv1alpha1.AgentProcessPhaseStopped
	case "exited", "succeeded", "failed":
		if exitCode != nil && *exitCode == 0 {
			return workspacesv1alpha1.AgentProcessPhaseSucceeded
		}
		return workspacesv1alpha1.AgentProcessPhaseFailed
	default:
		return workspacesv1alpha1.AgentProcessPhaseStarting
	}
}

func runtimeMessage(phase workspacesv1alpha1.AgentProcessPhase) string {
	switch phase {
	case workspacesv1alpha1.AgentProcessPhaseSucceeded:
		return "Process exited successfully"
	case workspacesv1alpha1.AgentProcessPhaseFailed:
		return "Process exited with a non-zero status"
	case workspacesv1alpha1.AgentProcessPhaseStopped:
		return "Process was stopped"
	default:
		return "Process reached a terminal state"
	}
}

func (r *AgentProcessReconciler) setProcessCondition(ctx context.Context, key types.NamespacedName, phase workspacesv1alpha1.AgentProcessPhase, status metav1.ConditionStatus, reason string, message string) error {
	current := new(workspacesv1alpha1.AgentProcess)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch AgentProcess before status update: %w", err)
	}
	current.Status.ObservedGeneration = current.Generation
	current.Status.Phase = phase
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.AgentProcessConditionReady, Status: status,
		ObservedGeneration: current.Generation, Reason: reason, Message: message,
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update AgentProcess status: %w", err)
	}

	return nil
}

func (r *AgentProcessReconciler) setTerminalProcessStatus(ctx context.Context, key types.NamespacedName, phase workspacesv1alpha1.AgentProcessPhase, exitCode *int32, reason string, message string, attachedClients int32) error {
	current := new(workspacesv1alpha1.AgentProcess)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch AgentProcess before terminal status update: %w", err)
	}
	now := metav1.Now()
	current.Status.ObservedGeneration = current.Generation
	current.Status.Phase = phase
	current.Status.CompletedAt = &now
	current.Status.ExitCode = exitCode
	current.Status.TerminationReason = reason
	current.Status.AttachedClients = attachedClients
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.AgentProcessConditionReady, Status: metav1.ConditionFalse,
		ObservedGeneration: current.Generation, Reason: string(phase), Message: message,
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update terminal AgentProcess status: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentProcessReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.AgentProcess{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.agentProcessesForRuntimePod)).
		Named("workspaces-agentprocess").
		Complete(r)
}

func (r *AgentProcessReconciler) agentProcessesForRuntimePod(ctx context.Context, object client.Object) []reconcile.Request {
	labels := object.GetLabels()
	if labels[workspaceManagedByLabel] == "" && labels[environmentManagedByLabel] == "" {
		return nil
	}
	processes := new(workspacesv1alpha1.AgentProcessList)
	if err := r.List(ctx, processes, client.InNamespace(object.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Could not list AgentProcesses for runtime Pod", "pod", object.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range processes.Items {
		process := &processes.Items[index]
		if agentProcessTerminal(process.Status.Phase) || process.Status.RuntimePodName != object.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(process)})
	}

	return requests
}
