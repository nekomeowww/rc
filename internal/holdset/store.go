package holdset

import (
	"context"
	"maps"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AnnotationStore keeps the hold set on an existing object, such as the
// Worktree it protects. It never creates the object: Save(nil) is ErrAbsent.
type AnnotationStore struct {
	Client client.Client
	// Reader is required and must bypass the manager cache in production.
	Reader client.Reader
	Key    client.ObjectKey
	// New returns an empty object of the stored type.
	New func() client.Object
	// UID, when set, treats any other incarnation as absent.
	UID        types.UID
	Annotation string
	// Decode overrides Decode, for example to read legacy annotations.
	Decode func(client.Object) (State, error)
	// Legacy annotations are removed on every Save.
	Legacy []string
}

// Load reads the object outside the cache.
func (s AnnotationStore) Load(ctx context.Context) (client.Object, State, error) {
	obj := s.New()
	if err := s.Reader.Get(ctx, s.Key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, State{}, nil
		}
		return nil, State{}, err
	}
	if s.UID != "" && obj.GetUID() != s.UID {
		return nil, State{}, nil
	}
	decode := s.Decode
	if decode == nil {
		decode = func(obj client.Object) (State, error) { return Decode(obj, s.Annotation) }
	}
	state, err := decode(obj)
	return obj, state, err
}

// Save patches only metadata, with a resourceVersion precondition.
func (s AnnotationStore) Save(ctx context.Context, obj client.Object, state State) error {
	if obj == nil {
		return ErrAbsent
	}
	before := obj.DeepCopyObject().(client.Object)
	if err := Encode(obj, s.Annotation, state); err != nil {
		return err
	}
	annotations := obj.GetAnnotations()
	for _, legacy := range s.Legacy {
		delete(annotations, legacy)
	}
	obj.SetAnnotations(annotations)
	return s.Client.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// LeaseStore keeps the hold set on a dedicated Lease, created on first use.
// Use it when the protected object must not be written by its consumers.
type LeaseStore struct {
	Client client.Client
	// Reader is required and must bypass the manager cache in production.
	Reader     client.Reader
	Key        client.ObjectKey
	Annotation string
	// Labels and OwnerReferences are set when the Lease is created.
	Labels          map[string]string
	OwnerReferences []metav1.OwnerReference
	// Decode overrides Decode, for example to read a legacy encoding.
	Decode func(client.Object) (State, error)
}

// Load reads the Lease outside the cache.
func (s LeaseStore) Load(ctx context.Context) (client.Object, State, error) {
	lease := new(coordinationv1.Lease)
	if err := s.Reader.Get(ctx, s.Key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, State{}, nil
		}
		return nil, State{}, err
	}
	decode := s.Decode
	if decode == nil {
		decode = func(obj client.Object) (State, error) { return Decode(obj, s.Annotation) }
	}
	state, err := decode(lease)
	return lease, state, err
}

// Save updates the Lease with its resourceVersion, or creates it. A racing
// create returns AlreadyExists, which Update retries like a Conflict.
func (s LeaseStore) Save(ctx context.Context, obj client.Object, state State) error {
	if obj == nil {
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: s.Key.Name, Namespace: s.Key.Namespace,
			Labels: maps.Clone(s.Labels), OwnerReferences: s.OwnerReferences,
		}}
		if err := Encode(lease, s.Annotation, state); err != nil {
			return err
		}
		return s.Client.Create(ctx, lease)
	}
	if err := Encode(obj, s.Annotation, state); err != nil {
		return err
	}
	return s.Client.Update(ctx, obj)
}
