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
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
)

const (
	environmentSudoersContainerName = "configure-sudo"
	environmentSudoersVolumeName    = "sudoers"
	environmentSudoersMountPath     = "/etc/sudoers.d"
	environmentSudoersFilePath      = environmentSudoersMountPath + "/agent"
	environmentSudoersRule          = "agent ALL=(ALL:ALL) NOPASSWD:ALL"
)

func (r *AgentProcessReconciler) resolveEnvironmentProcessTarget(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (*resolvedProcessTarget, string, string, error) {
	environment := new(workspacesv1alpha1.WorkspaceEnvironment)
	key := types.NamespacedName{Name: process.Spec.TargetRef.Name, Namespace: process.Namespace}
	if err := r.Get(ctx, key, environment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "TargetNotFound", "Target WorkspaceEnvironment does not exist", nil
		}
		return nil, "", "", fmt.Errorf("get target WorkspaceEnvironment: %w", err)
	}
	if !meta.IsStatusConditionTrue(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady) || environment.Status.CurrentVolumeClaimName == "" {
		return nil, reasonTargetNotReady, "Target WorkspaceEnvironment current revision is not ready", nil
	}

	draftName := environment.Status.DraftVolumeClaimName
	if draftName == "" {
		draftName = fmt.Sprintf("%s-draft-%d", environment.Name, environment.Status.CurrentRevision+1)
	}
	draft := new(corev1.PersistentVolumeClaim)
	draftKey := types.NamespacedName{Name: draftName, Namespace: environment.Namespace}
	err := r.Get(ctx, draftKey, draft)
	if apierrors.IsNotFound(err) {
		draft = environmentVolumeClaim(environment, draftName, environment.Status.CurrentVolumeClaimName)
		if err := controllerutil.SetControllerReference(environment, draft, r.Scheme); err != nil {
			return nil, "", "", fmt.Errorf("set WorkspaceEnvironment owner on draft PersistentVolumeClaim: %w", err)
		}
		if err := r.Create(ctx, draft); err != nil {
			return nil, "", "", fmt.Errorf("create WorkspaceEnvironment draft PersistentVolumeClaim: %w", err)
		}
		if err := r.setEnvironmentDraftStatus(ctx, key, draft.Name, "", metav1.ConditionFalse, "Provisioning", "Environment draft volume is provisioning"); err != nil {
			return nil, "", "", err
		}

		return nil, reasonTargetNotReady, "Target WorkspaceEnvironment draft is provisioning", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("get WorkspaceEnvironment draft PersistentVolumeClaim: %w", err)
	}
	if !environmentClaimMatches(draft, environment.Spec.Storage, environment.Status.CurrentVolumeClaimName) {
		return nil, "DraftVolumeMismatch", "Environment draft volume does not clone current", nil
	}
	if draft.Status.Phase != corev1.ClaimBound {
		if failureReason, failureMessage, failureErr := (&WorkspaceReconciler{Client: r.Client}).persistentVolumeClaimFailure(ctx, draft); failureErr != nil {
			return nil, "", "", failureErr
		} else if failureReason != "" {
			return nil, failureReason, failureMessage, nil
		}
		return nil, reasonTargetNotReady, "Target WorkspaceEnvironment draft is provisioning", nil
	}

	if err := (&WorkspaceReconciler{Client: r.Client}).ensureWorkspaceAccess(ctx, environment.Namespace); err != nil {
		return nil, "", "", err
	}
	editorName := environment.Name + "-editor"
	editor := new(corev1.Pod)
	editorKey := types.NamespacedName{Name: editorName, Namespace: environment.Namespace}
	err = r.Get(ctx, editorKey, editor)
	if apierrors.IsNotFound(err) {
		editor = environmentEditorPod(environment, draft.Name)
		if err := controllerutil.SetControllerReference(environment, editor, r.Scheme); err != nil {
			return nil, "", "", fmt.Errorf("set WorkspaceEnvironment owner on editor Pod: %w", err)
		}
		if err := r.Create(ctx, editor); err != nil {
			return nil, "", "", fmt.Errorf("create WorkspaceEnvironment editor Pod: %w", err)
		}
		if err := r.setEnvironmentDraftStatus(ctx, key, draft.Name, editor.Name, metav1.ConditionFalse, "Starting", "Environment editor Pod is starting"); err != nil {
			return nil, "", "", err
		}

		return nil, reasonTargetNotReady, "Target WorkspaceEnvironment editor is starting", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("get WorkspaceEnvironment editor Pod: %w", err)
	}
	if !podReady(editor) {
		return nil, reasonTargetNotReady, "Target WorkspaceEnvironment editor is starting", nil
	}
	if err := r.setEnvironmentDraftStatus(ctx, key, draft.Name, editor.Name, metav1.ConditionTrue, "DraftReady", "Environment draft editor is ready"); err != nil {
		return nil, "", "", err
	}

	workingDirectory := process.Spec.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = workspaceHomeMountPath
	}
	environmentVariables, err := r.resolveProcessOnlyEnvironment(ctx, process)
	if err != nil {
		return nil, "", "", err
	}
	agentHome, credentialFiles, err := r.resolveEnvironmentProcessCredentials(ctx, environment, process)
	if err != nil {
		return nil, "", "", err
	}

	return &resolvedProcessTarget{
		runtime: processruntime.Target{Namespace: environment.Namespace, Pod: editor.Name, Container: runtimeContainerName},
		podUID:  string(editor.UID), workingDir: workingDirectory, environment: environmentVariables,
		agentHome: agentHome, credentials: credentialFiles,
	}, "", "", nil
}

func environmentEditorPod(environment *workspacesv1alpha1.WorkspaceEnvironment, draftClaimName string) *corev1.Pod {
	runAsRoot := int64(0)
	runAsRootGroup := int64(0)
	automount := true
	allowPrivilegeEscalation := true
	disallowPrivilegeEscalation := false
	runAsNonRoot := false

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      environment.Name + "-editor",
			Namespace: environment.Namespace,
			Labels:    map[string]string{environmentManagedByLabel: environment.Name},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           defaultWorkspaceServiceAccount,
			AutomountServiceAccountToken: &automount,
			RestartPolicy:                corev1.RestartPolicyAlways,
			SecurityContext:              runtimepolicy.AgentPodSecurityContext(),
			InitContainers: []corev1.Container{{
				Name: environmentSudoersContainerName, Image: environment.Spec.Image,
				Command: []string{"/bin/sh", "-ec"},
				Args: []string{fmt.Sprintf(
					"chgrp 0 %[1]s && chmod 0755 %[1]s && printf '%%s\\n' %[2]q > %[3]s && chgrp 0 %[3]s && chmod 0440 %[3]s",
					environmentSudoersMountPath, environmentSudoersRule, environmentSudoersFilePath,
				)},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             &runAsNonRoot,
					RunAsUser:                &runAsRoot,
					RunAsGroup:               &runAsRootGroup,
					AllowPrivilegeEscalation: &disallowPrivilegeEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allLinuxCapabilities}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: environmentSudoersVolumeName, MountPath: environmentSudoersMountPath}},
			}},
			Containers: []corev1.Container{{
				Name: runtimeContainerName, Image: environment.Spec.Image, Command: []string{runtimeContainerName, runtimeServeArgument},
				Args: []string{"--socket", "/run/rc/rc-kube.sock", "--state-dir", "/home/agent/.rc/processes"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: workspaceHomeVolumeName, MountPath: workspaceHomeMountPath},
					{Name: workspaceRuntimeVolumeName, MountPath: "/run/rc"},
					{Name: environmentSudoersVolumeName, MountPath: environmentSudoersMountPath, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: workspaceHomeVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: draftClaimName}}},
				{Name: workspaceRuntimeVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: environmentSudoersVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

func (r *AgentProcessReconciler) setEnvironmentDraftStatus(ctx context.Context, key types.NamespacedName, draftName string, editorName string, status metav1.ConditionStatus, reason string, message string) error {
	current := new(workspacesv1alpha1.WorkspaceEnvironment)
	if err := r.Get(ctx, key, current); err != nil {
		return fmt.Errorf("re-fetch WorkspaceEnvironment before draft status update: %w", err)
	}
	current.Status.DraftVolumeClaimName = draftName
	current.Status.EditorPodName = editorName
	meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: workspacesv1alpha1.WorkspaceEnvironmentConditionDraftReady, Status: status,
		ObservedGeneration: current.Generation, Reason: reason, Message: message,
	})
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("update WorkspaceEnvironment draft status: %w", err)
	}

	return nil
}

func (r *AgentProcessReconciler) resolveProcessOnlyEnvironment(ctx context.Context, process *workspacesv1alpha1.AgentProcess) (map[string]string, error) {
	values := make(map[string]string, len(process.Spec.Env))
	if process.Spec.EnvSecretRef == nil {
		return values, nil
	}
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

	return values, nil
}

func (r *AgentProcessReconciler) resolveEnvironmentProcessCredentials(ctx context.Context, environment *workspacesv1alpha1.WorkspaceEnvironment, process *workspacesv1alpha1.AgentProcess) (string, map[string][]byte, error) {
	files := make(map[string][]byte)
	agentHome := ""
	if process.Spec.AgentType != "" {
		credentialName := defaultCredentialName
		if process.Spec.AgentCredentialRef != nil {
			credentialName = process.Spec.AgentCredentialRef.Name
		}
		agentHome = filepath.Join(workspaceHomeMountPath, ".rc", "agents", process.Spec.AgentType, credentialName)
	}
	if process.Spec.AgentCredentialRef != nil {
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: environment.Name, Namespace: environment.Namespace},
			Spec: workspacesv1alpha1.WorkspaceSpec{
				AgentCredentialRefs: []workspacesv1alpha1.LocalReference{*process.Spec.AgentCredentialRef},
				CredentialRefs:      append([]workspacesv1alpha1.LocalReference(nil), process.Spec.CredentialRefs...),
			},
		}
		_, files, err := r.resolveProcessCredentials(ctx, workspace, process)
		return agentHome, files, err
	}
	for _, reference := range process.Spec.CredentialRefs {
		credentialFiles, err := r.resolveGenericCredential(ctx, environment.Namespace, reference.Name)
		if err != nil {
			return "", nil, err
		}
		for name, data := range credentialFiles {
			files[filepath.Join("credentials", reference.Name, name)] = data
		}
	}

	return agentHome, files, nil
}
