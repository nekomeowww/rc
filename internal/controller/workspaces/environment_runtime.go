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
	"path"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/rcplatform"
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
	ready := meta.FindStatusCondition(environment.Status.Conditions, workspacesv1alpha1.WorkspaceEnvironmentConditionReady)
	if environment.Status.ObservedGeneration < environment.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration < environment.Generation || environment.Status.CurrentVolumeClaimName == "" {
		return nil, reasonTargetNotReady, "Target WorkspaceEnvironment current revision is not ready", nil
	}

	platform, platformErr := rcplatform.Resolve(rcplatform.Target{OS: environment.Spec.OS, Placement: rcplatform.Placement{
		NodeSelector: environment.Spec.NodeSelector, Tolerations: environment.Spec.Tolerations,
	}})
	if platformErr != nil {
		reason, message := runtimePlatformCondition(platformErr)
		return nil, reason, message, nil
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
	editor, reason, message, err := r.ensureEnvironmentEditor(ctx, environment, draft, platform)
	if err != nil || reason != "" {
		return nil, reason, message, err
	}

	environmentVariables, err := r.resolveProcessOnlyEnvironment(ctx, process)
	if err != nil {
		return nil, "", "", err
	}
	agentProfile, credentialFiles, err := r.resolveEnvironmentProcessCredentials(ctx, environment, process)
	if err != nil {
		return nil, "", "", err
	}
	credentialProjection, err := r.resolveCredentialProjections(ctx, process.Namespace, process.Spec.CredentialRefs)
	if err != nil {
		return nil, "", "", err
	}

	return &resolvedProcessTarget{
		platform: platform, runtime: platform.ProcessTarget(environment.Namespace, editor.Name, runtimeContainerName),
		podUID: string(editor.UID), workingDir: process.Spec.WorkingDirectory, defaultDirectory: rcplatform.HomeDirectory, environment: environmentVariables,
		agentProfile: agentProfile, credentials: credentialFiles, mounts: credentialProjection.mounts, credentialEnvironment: credentialProjection.environment,
		sshConfigFragments: credentialProjection.sshConfigFragments,
	}, "", "", nil
}

func (r *AgentProcessReconciler) ensureEnvironmentEditor(ctx context.Context, environment *workspacesv1alpha1.WorkspaceEnvironment, draft *corev1.PersistentVolumeClaim, platform rcplatform.Runtime) (*corev1.Pod, string, string, error) {
	editorName := environment.Name + "-editor"
	editor := new(corev1.Pod)
	key := types.NamespacedName{Name: editorName, Namespace: environment.Namespace}
	err := r.Get(ctx, key, editor)
	if apierrors.IsNotFound(err) {
		editor, err = platform.EnvironmentEditorPod(rcplatform.EditorPodIntent{
			Metadata: metav1.ObjectMeta{Name: editorName, Namespace: environment.Namespace, Labels: map[string]string{environmentManagedByLabel: environment.Name}},
			Image:    environment.Spec.Image, HomeClaim: draft.Name, ServiceAccount: defaultWorkspaceServiceAccount,
		})
		if err != nil {
			return nil, "", "", fmt.Errorf("build WorkspaceEnvironment editor Pod: %w", err)
		}
		if err := controllerutil.SetControllerReference(environment, editor, r.Scheme); err != nil {
			return nil, "", "", fmt.Errorf("set WorkspaceEnvironment owner on editor Pod: %w", err)
		}
		if err := r.Create(ctx, editor); err != nil {
			return nil, "", "", fmt.Errorf("create WorkspaceEnvironment editor Pod: %w", err)
		}
		environmentKey := types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}
		if err := r.setEnvironmentDraftStatus(ctx, environmentKey, draft.Name, editor.Name, metav1.ConditionFalse, "Starting", "Environment editor Pod is starting"); err != nil {
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
	environmentKey := types.NamespacedName{Name: environment.Name, Namespace: environment.Namespace}
	if err := r.setEnvironmentDraftStatus(ctx, environmentKey, draft.Name, editor.Name, metav1.ConditionTrue, "DraftReady", "Environment draft editor is ready"); err != nil {
		return nil, "", "", err
	}
	return editor, "", "", nil
}

func environmentEditorPod(environment *workspacesv1alpha1.WorkspaceEnvironment, draftClaimName string) (*corev1.Pod, error) {
	platform, err := rcplatform.Resolve(rcplatform.Target{OS: environment.Spec.OS, Placement: rcplatform.Placement{
		NodeSelector: environment.Spec.NodeSelector, Tolerations: environment.Spec.Tolerations,
	}})
	if err != nil {
		return nil, err
	}
	return platform.EnvironmentEditorPod(rcplatform.EditorPodIntent{
		Metadata: metav1.ObjectMeta{Name: environment.Name + "-editor", Namespace: environment.Namespace, Labels: map[string]string{environmentManagedByLabel: environment.Name}},
		Image:    environment.Spec.Image, HomeClaim: draftClaimName, ServiceAccount: defaultWorkspaceServiceAccount,
	})
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

func (r *AgentProcessReconciler) resolveEnvironmentProcessCredentials(ctx context.Context, environment *workspacesv1alpha1.WorkspaceEnvironment, process *workspacesv1alpha1.AgentProcess) (*rcplatform.AgentProfile, map[string][]byte, error) {
	files := make(map[string][]byte)
	var agentProfile *rcplatform.AgentProfile
	if process.Spec.AgentType != "" {
		credentialName := defaultCredentialName
		if process.Spec.AgentCredentialRef != nil {
			credentialName = process.Spec.AgentCredentialRef.Name
		}
		agentProfile = &rcplatform.AgentProfile{Type: process.Spec.AgentType, Credential: credentialName}
	}
	if process.Spec.AgentCredentialRef != nil {
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: environment.Name, Namespace: environment.Namespace},
			Spec: workspacesv1alpha1.WorkspaceSpec{
				OS: environment.Spec.OS, AgentCredentialRefs: []workspacesv1alpha1.LocalReference{*process.Spec.AgentCredentialRef},
				CredentialRefs: append([]workspacesv1alpha1.LocalReference(nil), process.Spec.CredentialRefs...),
			},
		}
		_, files, err := r.resolveProcessCredentials(ctx, workspace, process)
		return agentProfile, files, err
	}
	for _, reference := range process.Spec.CredentialRefs {
		credentialFiles, err := r.resolveGenericCredential(ctx, environment.Namespace, reference.Name)
		if err != nil {
			return nil, nil, err
		}
		for name, data := range credentialFiles {
			files[path.Join("credentials", reference.Name, name)] = data
		}
	}

	return agentProfile, files, nil
}
