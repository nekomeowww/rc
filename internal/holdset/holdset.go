// Package holdset records which resources hold a shared Kubernetes object.
//
// A hold set is a durable list of holders plus an irreversible Closed fence,
// stored in one annotation of one object. Every change is a compare-and-swap on
// that object's resourceVersion, so admission and the deletion fence serialize
// on the same write. Holders have no TTL: time passing cannot stop a consumer.
// Owners release their holder after their consumers stop; Sweep removes holders
// whose owner is gone and whose consumers are provably stopped.
package holdset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Mode is a holder's access mode. Its meaning belongs to the Conflicts policy.
type Mode string

// Holder is one resource incarnation that holds the object.
type Holder struct {
	Kind  string      `json:"kind"`
	Name  string      `json:"name"`
	UID   types.UID   `json:"uid"`
	Mode  Mode        `json:"mode"`
	Since metav1.Time `json:"since"`
}

// HolderFor identifies owner by UID, so name reuse never inherits a hold.
func HolderFor(kind string, owner client.Object, mode Mode) Holder {
	return Holder{Kind: kind, Name: owner.GetName(), UID: owner.GetUID(), Mode: mode}
}

// Key identifies the holder independently of its mode and start time.
func (h Holder) Key() string {
	return h.Kind + "/" + string(h.UID)
}

// State is the decoded hold set.
type State struct {
	Closed  bool     `json:"closed,omitempty"`
	Holders []Holder `json:"holders,omitempty"`
}

// Find returns the holder with key.
func (s State) Find(key string) (Holder, bool) {
	index := slices.IndexFunc(s.Holders, func(h Holder) bool { return h.Key() == key })
	if index < 0 {
		return Holder{}, false
	}
	return s.Holders[index], true
}

// Remove drops key and reports whether it was present.
func (s *State) Remove(key string) bool {
	before := len(s.Holders)
	s.Holders = slices.DeleteFunc(s.Holders, func(h Holder) bool { return h.Key() == key })
	return len(s.Holders) != before
}

// Put records h, replacing an existing holder with the same key. The original
// Since is kept, so a mode change does not reset how long a holder has held.
func (s *State) Put(h Holder) {
	if existing, found := s.Find(h.Key()); found && !existing.Since.IsZero() {
		h.Since = existing.Since
	}
	if h.Since.IsZero() {
		h.Since = metav1.Now().Rfc3339Copy()
	}
	s.Remove(h.Key())
	s.Holders = append(s.Holders, h)
	slices.SortFunc(s.Holders, func(left, right Holder) int { return strings.Compare(left.Key(), right.Key()) })
}

// Conflicts returns the existing holders that exclude candidate. existing
// never contains candidate's own key. An empty result admits the candidate.
type Conflicts func(existing []Holder, candidate Holder) []Holder

// Result describes one admission decision.
type Result struct {
	// Admitted permits the candidate to create new consumers.
	Admitted bool
	// Held reports that the candidate's key already held the requested mode,
	// even when the set is now Closed and admits nothing new.
	Held bool
	// Closed reports that the fence refused the candidate.
	Closed bool
	// Conflicts lists holders whose modes excluded the candidate.
	Conflicts []Holder
}

// Admit applies admission to s in memory and reports whether s changed.
// A Closed set admits no new holder and no mode change.
func (s *State) Admit(candidate Holder, conflicts Conflicts) (Result, bool) {
	if existing, found := s.Find(candidate.Key()); found && existing.Mode == candidate.Mode {
		return Result{Admitted: !s.Closed, Held: true, Closed: s.Closed}, false
	}
	if s.Closed {
		return Result{Closed: true}, false
	}
	others := slices.DeleteFunc(slices.Clone(s.Holders), func(h Holder) bool { return h.Key() == candidate.Key() })
	if blocking := conflicts(others, candidate); len(blocking) > 0 {
		return Result{Conflicts: blocking}, false
	}
	s.Put(candidate)
	return Result{Admitted: true}, true
}

// ErrAbsent means the backing object does not exist, or is a different
// incarnation than the store was built for. Stores that cannot create their
// object return it from Save(nil).
var ErrAbsent = errors.New("hold set object is absent")

// Store loads and saves one hold set. Load must bypass informer caches; Save
// must fail with a Conflict (or AlreadyExists) when obj is no longer current.
type Store interface {
	// Load returns nil obj when the object is absent.
	Load(ctx context.Context) (client.Object, State, error)
	// Save persists s. A nil obj asks the store to create its object.
	Save(ctx context.Context, obj client.Object, s State) error
}

// Update applies change under optimistic concurrency and returns the state
// that was persisted, or the loaded state when change reported no change.
// On error the write may have succeeded: callers must treat the outcome as unknown.
func Update(ctx context.Context, store Store, change func(obj client.Object, s *State) (bool, error)) (State, error) {
	var result State
	err := retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error {
		obj, current, err := store.Load(ctx)
		if err != nil {
			return err
		}
		changed, err := change(obj, &current)
		if err != nil {
			return err
		}
		result = current
		if !changed {
			return nil
		}
		return store.Save(ctx, obj, current)
	})
	return result, err
}

// Acquire records candidate before it creates any consumer. Admission and
// Close CAS the same object, so a racing Close makes this write fail and the
// retry observes the fence. An absent object admits nothing.
func Acquire(ctx context.Context, store Store, candidate Holder, conflicts Conflicts) (Result, error) {
	var result Result
	_, err := Update(ctx, store, func(_ client.Object, s *State) (bool, error) {
		var changed bool
		result, changed = s.Admit(candidate, conflicts)
		return changed, nil
	})
	if errors.Is(err, ErrAbsent) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// Release removes key. Call it only after the holder's consumers have stopped.
// Releasing the last holder never reopens a Closed set.
func Release(ctx context.Context, store Store, key string) error {
	_, err := Update(ctx, store, func(_ client.Object, s *State) (bool, error) {
		return s.Remove(key), nil
	})
	return err
}

// Close fences the set permanently and returns the holders that remain. An
// empty result means every admitted creator has released; new admissions
// after Close are impossible.
func Close(ctx context.Context, store Store) ([]Holder, error) {
	s, err := Update(ctx, store, func(obj client.Object, s *State) (bool, error) {
		if obj == nil {
			return false, ErrAbsent
		}
		if s.Closed {
			return false, nil
		}
		s.Closed = true
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return s.Holders, nil
}

// Sweep removes holders for which stale reports true and returns them. stale
// runs outside the CAS loop and must be conservative: a holder is stale only
// when its owner is gone and no consumer it may have created still runs.
func Sweep(ctx context.Context, store Store, stale func(Holder) (bool, error)) ([]Holder, error) {
	_, current, err := store.Load(ctx)
	if err != nil || len(current.Holders) == 0 {
		return nil, err
	}
	candidates := make([]Holder, 0)
	for _, holder := range current.Holders {
		gone, err := stale(holder)
		if err != nil {
			return nil, err
		}
		if gone {
			candidates = append(candidates, holder)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	var removed []Holder
	_, err = Update(ctx, store, func(_ client.Object, s *State) (bool, error) {
		removed = removed[:0]
		for _, holder := range candidates {
			// UIDs are never reused, so a holder whose owner is gone cannot have
			// been re-admitted between the evaluation and this write.
			if s.Remove(holder.Key()) {
				removed = append(removed, holder)
			}
		}
		return len(removed) > 0, nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// Decode reads the hold set stored in annotation. Missing means empty.
func Decode(obj client.Object, annotation string) (State, error) {
	s := State{}
	value := obj.GetAnnotations()[annotation]
	if value == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(value), &s); err != nil {
		return State{}, fmt.Errorf("decode hold set %s: %w", annotation, err)
	}
	return s, nil
}

// Encode stores s in annotation on obj.
func Encode(obj client.Object, annotation string, s State) error {
	encoded, err := json.Marshal(s)
	if err != nil {
		return err
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[annotation] = string(encoded)
	obj.SetAnnotations(annotations)
	return nil
}
