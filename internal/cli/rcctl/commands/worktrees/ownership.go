package worktrees

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/cli/rcctl/cluster"
	"github.com/nekomeowww/rc/internal/kubeconfig"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
	"github.com/nekomeowww/rc/internal/worktreeownership"
)

// newOwnershipCommand exposes explicit persistence transitions; detach here is
// unrelated to the Git checkout --detach flag on worktree add.
func newOwnershipCommand(flags *kubeconfig.Flags, adopt bool) *cobra.Command {
	verb, description := "detach", "Make a Workspace-owned Worktree independent before deleting its owner"
	if adopt {
		verb, description = "adopt", "Make an independent Worktree cascade with a named Workspace"
	}
	var workspace string
	cmd := &cobra.Command{Use: verb + " NAME --workspace NAME", Short: description, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if workspace == "" {
			return fmt.Errorf("--workspace is required")
		}
		config, namespace, err := flags.Resolve()
		if err != nil {
			return err
		}
		clusterClient, err := cluster.New(config)
		if err != nil {
			return err
		}
		if err := changeWorktreeOwnership(cmd.Context(), clusterClient.Kube, namespace, args[0], workspace, adopt); err != nil {
			return err
		}
		if adopt {
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "worktree/%s adopted by workspace/%s; it will cascade when that Workspace is deleted\n", args[0], workspace)
		} else {
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "worktree/%s detached from workspace/%s; it will be retained independently\n", args[0], workspace)
		}
		return err
	}}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Explicit Workspace owner (required; never uses the current default)")
	return cmd
}

// changeWorktreeOwnership changes metadata only, preserving checkout, mounts,
// PVC, and writer Leases. Optimistic locking rejects concurrent deletion or
// ownership changes rather than overwriting a newer object.
func changeWorktreeOwnership(ctx context.Context, kube client.Client, namespace, name, workspaceName string, adopt bool) error {
	worktree := new(repositoriesv1alpha1.Worktree)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, worktree); err != nil {
		return err
	}
	workspace := new(workspacesv1alpha1.Workspace)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: workspaceName}, workspace); err != nil {
		return err
	}
	if !workspace.DeletionTimestamp.IsZero() || !worktree.DeletionTimestamp.IsZero() {
		return fmt.Errorf("ownership cannot change while the Workspace or Worktree is being deleted")
	}
	if workspace.UID == "" {
		return fmt.Errorf("workspace %q has no UID", workspaceName)
	}
	ready := meta.FindStatusCondition(worktree.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration < worktree.Generation || worktree.Status.ObservedGeneration < worktree.Generation {
		return fmt.Errorf("worktree %q must be Ready before changing ownership; finish its checkout first", name)
	}
	before := worktree.DeepCopy()
	if err := updateWorktreeOwnership(worktree, workspace, adopt); err != nil {
		return err
	}
	if adopt {
		if err := worktreeownership.ProtectVolume(ctx, kube, worktree); err != nil {
			return err
		}
	}
	controllerutil.AddFinalizer(worktree, worktreeclaim.DeletionFinalizer)
	return kube.Patch(ctx, worktree, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func updateWorktreeOwnership(worktree *repositoriesv1alpha1.Worktree, workspace *workspacesv1alpha1.Workspace, adopt bool) error {
	if adopt {
		if worktreeownership.IsOwnedBy(worktree, workspace) && len(worktree.OwnerReferences) == 1 {
			return nil
		}
		if len(worktree.OwnerReferences) != 0 {
			return fmt.Errorf("worktree %q already has an owner; detach it explicitly first", worktree.Name)
		}
		worktree.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workspace, workspacesv1alpha1.SchemeGroupVersion.WithKind("Workspace"))}
		// Do not add bootstrap hints when adopting a user-created checkout.
		return nil
	}
	if !worktreeownership.IsOwnedBy(worktree, workspace) || len(worktree.OwnerReferences) != 1 {
		return fmt.Errorf("worktree %q is not exclusively owned by this Workspace UID", worktree.Name)
	}
	worktree.OwnerReferences = nil
	// Ready checkouts no longer need the legacy bootstrap hint. Removing it also
	// prevents old clients' label-based cascade from deleting a detached asset.
	delete(worktree.Labels, worktreeownership.GeneratedForLabel)
	return nil
}
