package worktreeownership

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/holdset"
)

const (
	// HoldersAnnotation stores the Worktree hold set: every admitted Workspace
	// mount and writer, plus the irreversible storage/deletion fence.
	HoldersAnnotation = "repositories.rc.ayaka.io/holders"
	// Annotations written before the hold set. They are still read, and are
	// removed by the first hold set write, for one release.
	legacyMountHoldersAnnotation = "repositories.rc.ayaka.io/mount-holders"
	legacyMountsClosedAnnotation = "repositories.rc.ayaka.io/mounts-closed"
)

// Worktree holder modes. Readers coexist with everything; a writer excludes
// other writers. Deletion uses the Closed fence, not a holder.
const (
	Read  holdset.Mode = "read"
	Write holdset.Mode = "write"
)

// Holder kinds on a Worktree.
const (
	KindWorkspace    = "Workspace"
	KindWorktreeExec = "WorktreeExec"
)

// Conflicts admits any number of readers and at most one writer.
func Conflicts(existing []holdset.Holder, candidate holdset.Holder) []holdset.Holder {
	if candidate.Mode != Write {
		return nil
	}
	var blocking []holdset.Holder
	for _, holder := range existing {
		if holder.Mode == Write {
			blocking = append(blocking, holder)
		}
	}
	return blocking
}

// WorkspaceHolder identifies one Workspace incarnation's use of a Worktree.
func WorkspaceHolder(workspace *workspacesv1alpha1.Workspace, mode holdset.Mode) holdset.Holder {
	return holdset.HolderFor(KindWorkspace, workspace, mode)
}

// Decode reads the Worktree hold set, merging the legacy mount-holders and
// mounts-closed annotations. Legacy mount holders were Workspaces of any
// mode; their write access was a separate Lease, imported by the controller.
func Decode(worktree *repositoriesv1alpha1.Worktree) (holdset.State, error) {
	state, err := holdset.Decode(worktree, HoldersAnnotation)
	if err != nil {
		return holdset.State{}, err
	}
	if worktree.Annotations[legacyMountsClosedAnnotation] != "" {
		state.Closed = true
	}
	if data := worktree.Annotations[legacyMountHoldersAnnotation]; data != "" {
		tokens := make(map[string]bool)
		if err := json.Unmarshal([]byte(data), &tokens); err != nil {
			return holdset.State{}, fmt.Errorf("decode legacy Worktree mount holders: %w", err)
		}
		for token, held := range tokens {
			uid, name, _ := strings.Cut(token, "/")
			holder := holdset.Holder{Kind: KindWorkspace, Name: name, UID: types.UID(uid), Mode: Read, Since: worktree.CreationTimestamp}
			if _, exists := state.Find(holder.Key()); held && uid != "" && !exists {
				state.Put(holder)
			}
		}
	}
	return state, nil
}

// MountsClosed reports an irreversible storage/deletion fence. A Worktree whose
// storage was explicitly deleted must not silently become a fresh checkout. An
// unreadable hold set fails closed.
func MountsClosed(worktree *repositoriesv1alpha1.Worktree) bool {
	if !worktree.DeletionTimestamp.IsZero() {
		return true
	}
	state, err := Decode(worktree)
	return err != nil || state.Closed
}

// MountAccess serializes runtime admission, writers and deletion on the
// Worktree itself. Every side uses resourceVersion preconditions on this same
// object, so a Lease that foreground GC can remove is never the fence.
// Workspace spec is a desired mount, not permission to create a consumer.
// Holders have no TTL: time passing cannot stop an admitted runtime.
type MountAccess struct {
	Client client.Client
	// Reader is required and must bypass the manager cache in production.
	Reader client.Reader
}

// Store returns the hold set of a Worktree. A non-empty uid treats any other
// incarnation as absent, so a stale capture cannot touch a replacement.
func (g MountAccess) Store(key client.ObjectKey, uid types.UID) holdset.AnnotationStore {
	return holdset.AnnotationStore{
		Client: g.Client, Reader: g.Reader, Key: key, UID: uid, Annotation: HoldersAnnotation,
		New:    func() client.Object { return new(repositoriesv1alpha1.Worktree) },
		Decode: func(obj client.Object) (holdset.State, error) { return Decode(obj.(*repositoriesv1alpha1.Worktree)) },
		Legacy: []string{legacyMountHoldersAnnotation, legacyMountsClosedAnnotation},
	}
}

// Admit records holder before owner creates any runtime, hot-mount helper or
// Job that uses the Worktree. The captured UID, generation and claim prevent a
// stale observation from admitting against changed storage; a DELETE or Close
// racing this patch changes resourceVersion, so the retry observes the fence.
// A refused result still reports Held when holder already holds its mode.
// On error the write may have succeeded: retain and later release the holder
// only after confirming that the owner's consumers have stopped.
func (g MountAccess) Admit(ctx context.Context, captured *repositoriesv1alpha1.Worktree, owner client.Object, holder holdset.Holder) (holdset.Result, error) {
	live := owner.DeepCopyObject().(client.Object)
	if err := g.Reader.Get(ctx, client.ObjectKeyFromObject(owner), live); err != nil {
		return holdset.Result{}, client.IgnoreNotFound(err)
	}
	if live.GetUID() != holder.UID || !live.GetDeletionTimestamp().IsZero() {
		return holdset.Result{}, nil
	}
	var result holdset.Result
	_, err := holdset.Update(ctx, g.Store(client.ObjectKeyFromObject(captured), captured.UID), func(obj client.Object, state *holdset.State) (bool, error) {
		result = holdset.Result{}
		if obj == nil {
			return false, nil
		}
		current := obj.(*repositoriesv1alpha1.Worktree)
		if current.Generation != captured.Generation || current.Status.VolumeClaimName != captured.Status.VolumeClaimName {
			existing, found := state.Find(holder.Key())
			result.Held = found && existing.Mode == holder.Mode
			return false, nil
		}
		admission := *state
		admission.Closed = admission.Closed || !current.DeletionTimestamp.IsZero()
		var changed bool
		result, changed = admission.Admit(holder, Conflicts)
		if changed {
			state.Holders = admission.Holders
		}
		return changed, nil
	})
	return result, err
}

// Close closes admission permanently and returns the holders that remain.
// Even with no visible Pod yet, an admitted creator keeps deletion waiting.
// New spec references after this point cannot obtain access.
func (g MountAccess) Close(ctx context.Context, worktree *repositoriesv1alpha1.Worktree) ([]holdset.Holder, error) {
	remaining, err := holdset.Close(ctx, g.Store(client.ObjectKeyFromObject(worktree), worktree.UID))
	if err != nil {
		return nil, fmt.Errorf("close Worktree admission: %w", err)
	}
	return remaining, nil
}

// Release drops one holder from the Worktree named key. Call it only after the
// holder's consumers have stopped. An empty uid accepts any incarnation; a
// holder key never matches an incarnation it was not admitted to.
func (g MountAccess) Release(ctx context.Context, key client.ObjectKey, uid types.UID, holderKey string) error {
	return holdset.Release(ctx, g.Store(key, uid), holderKey)
}

// ReleaseExcept drops this Workspace incarnation's unused holders. Call only
// after removed mounts' Pods have disappeared. Candidates are captured by the
// caller during topology change or teardown, never discovered on the Ready
// path. keep contains names still used by the current topology; nil releases all.
func (g MountAccess) ReleaseExcept(ctx context.Context, workspace *workspacesv1alpha1.Workspace, candidates []repositoriesv1alpha1.Worktree, keep map[string]bool) error {
	modes := make(map[string]holdset.Mode, len(keep))
	for name, kept := range keep {
		if kept {
			modes[name] = Write
		}
	}
	return g.Retain(ctx, workspace, candidates, modes)
}

// Retain releases this Workspace's holders on candidates absent from keep and
// downgrades a write holder to read where keep allows only reading. It never
// upgrades: writes are admitted only through Admit.
func (g MountAccess) Retain(ctx context.Context, workspace *workspacesv1alpha1.Workspace, candidates []repositoriesv1alpha1.Worktree, keep map[string]holdset.Mode) error {
	key := WorkspaceHolder(workspace, Read).Key()
	for _, worktree := range candidates {
		if worktree.Namespace != workspace.Namespace {
			continue
		}
		mode, kept := keep[worktree.Name]
		if kept && mode == Write {
			continue
		}
		if _, err := holdset.Update(ctx, g.Store(client.ObjectKeyFromObject(&worktree), worktree.UID), func(_ client.Object, state *holdset.State) (bool, error) {
			existing, found := state.Find(key)
			switch {
			case !found:
				return false, nil
			case !kept:
				return state.Remove(key), nil
			case existing.Mode == Write:
				existing.Mode = Read
				state.Put(existing)
				return true, nil
			default:
				return false, nil
			}
		}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
