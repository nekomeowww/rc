package workspaces

import (
	"context"
	"fmt"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/rcplatform"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TranscriptVolume identifies the original home and the platform that can read it.
type TranscriptVolume struct {
	Claim, Image string
	Runtime      rcplatform.Runtime
}

// ResolveTranscriptVolume finds the volume holding a process transcript when
// its runtime is not running: the target's current home or draft volume.
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
	platform, err := rcplatform.Resolve(target)
	volume.Runtime = platform
	return volume, err
}
