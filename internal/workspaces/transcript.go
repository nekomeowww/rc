package workspaces

import (
	"context"
	"fmt"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/rcplatform"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TranscriptVolume identifies the original home and the platform that can read it.
type TranscriptVolume struct {
	Claim, Image string
	Runtime      rcplatform.Runtime
}

// ResolveTranscriptVolume shares volume resolution between logs and cleanup.
// New executions pin the original PVC; legacy executions retain the historical
// target-based fallback. PVC UID mismatches must never touch replacement data.
func ResolveTranscriptVolume(ctx context.Context, reader client.Reader, process *workspacesv1alpha1.WorkspaceExec) (TranscriptVolume, error) {
	key := client.ObjectKey{Namespace: process.Namespace, Name: process.Spec.TargetRef.Name}
	volume := TranscriptVolume{}
	var target rcplatform.Target
	switch process.Spec.TargetRef.Kind {
	case workspacesv1alpha1.WorkspaceExecTargetWorkspace:
		workspace := new(workspacesv1alpha1.Workspace)
		if err := reader.Get(ctx, key, workspace); err != nil {
			return volume, err
		}
		volume.Claim, volume.Image = workspace.Status.HomeVolumeClaimName, workspace.Status.RuntimeImage
		target = rcplatform.Target{OS: workspace.Spec.OS, Placement: rcplatform.Placement{NodeSelector: workspace.Spec.NodeSelector, Tolerations: workspace.Spec.Tolerations, Affinity: workspace.Spec.Affinity, RuntimeClassName: workspace.Spec.RuntimeClassName}}
	case workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment:
		environment := new(workspacesv1alpha1.WorkspaceEnvironment)
		if err := reader.Get(ctx, key, environment); err != nil {
			return volume, err
		}
		volume.Claim, volume.Image = environment.Status.DraftVolumeClaimName, environment.Spec.Image
		if volume.Claim == "" {
			volume.Claim = environment.Status.CurrentVolumeClaimName
		}
		target = rcplatform.Target{OS: environment.Spec.OS, Placement: rcplatform.Placement{NodeSelector: environment.Spec.NodeSelector, Tolerations: environment.Spec.Tolerations}}
	default:
		return volume, fmt.Errorf("unsupported transcript target kind %s", process.Spec.TargetRef.Kind)
	}
	if process.Status.TranscriptVolumeClaimName != "" {
		volume.Claim = process.Status.TranscriptVolumeClaimName
		claim := new(corev1.PersistentVolumeClaim)
		if err := reader.Get(ctx, client.ObjectKey{Namespace: process.Namespace, Name: volume.Claim}, claim); err != nil {
			return volume, err
		}
		if process.Status.TranscriptVolumeClaimUID != "" && string(claim.UID) != process.Status.TranscriptVolumeClaimUID {
			return volume, fmt.Errorf("original transcript PVC was replaced")
		}
	}
	platform, err := rcplatform.Resolve(target)
	volume.Runtime = platform
	return volume, err
}
