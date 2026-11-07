package workspaces

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// imageChangePending reports whether the runtime uses an image other than the
// desired image of a blank Workspace.
func (resolved *resolvedWorkspace) imageChangePending() bool {
	return resolved.desiredImage != "" && resolved.desiredImage != resolved.image
}

// moveToDesiredImage moves a blank Workspace that has no runtime Pod to its
// desired image, as ADR 0011 decides. It records status.runtimeImage before
// the controller creates the Pod, so a failed status write cannot leave a Pod
// on an image that the topology hash does not expect. A running Pod keeps its
// image until it stops.
func (r *WorkspaceReconciler) moveToDesiredImage(ctx context.Context, workspace *workspacesv1alpha1.Workspace, resolved *resolvedWorkspace) error {
	// An empty status.runtimeImage is recorded by the first status write.
	if !resolved.imageChangePending() || workspace.Status.RuntimeImage == "" {
		return nil
	}
	if workspace.Spec.DesiredState == workspacesv1alpha1.WorkspaceDesiredStateSuspended {
		return nil
	}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(workspace), new(corev1.Pod)); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get Workspace runtime Pod before an image change: %w", err)
	}
	current := new(workspacesv1alpha1.Workspace)
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		return fmt.Errorf("re-fetch Workspace before an image change: %w", err)
	}
	previous := current.Status.RuntimeImage
	current.Status.RuntimeImage = resolved.desiredImage
	if err := r.Status().Update(ctx, current); err != nil {
		return fmt.Errorf("record Workspace runtime image: %w", err)
	}
	logf.FromContext(ctx).Info("Changed Workspace runtime image", "previous", previous, "image", resolved.desiredImage)
	workspace.Status.RuntimeImage = resolved.desiredImage
	resolved.image = resolved.desiredImage

	return nil
}
