package audit

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/repositoryaccess"
)

const (
	repoAPI                  = "repositories.rc.ayaka.io/v1alpha1"
	workspaceAPI             = "workspaces.rc.ayaka.io/v1alpha1"
	pvcKind                  = "PersistentVolumeClaim"
	worktreeKind             = "Worktree"
	workspaceKind            = "Workspace"
	workspaceEnvironmentKind = "WorkspaceEnvironment"
	workspaceExecKind        = "WorkspaceExec"
	workspaceExecFinalizer   = "workspaces.rc.ayaka.io/workspace-exec"
	leaseKind                = "Lease"
	podKind                  = "Pod"
	jobKind                  = "Job"
	ownerRelation            = "owner"
	pvcRelation              = "storage"
	runtimeRelation          = "runtime"
	mountRelation            = "mount"
)

// Scan reads all evidence in a namespace (or all namespaces when empty).
// Callers must provide an uncached client.Reader. Unavailable lists become
// unknown observations; cancellation and invalid policy are returned as errors.
func Scan(ctx context.Context, reader client.Reader, namespace string, policy Policy, now time.Time) (Report, error) {
	if err := validatePolicy(policy); err != nil {
		return Report{}, err
	}
	inventory, err := scanInventory(ctx, reader, namespace, now)
	if err != nil {
		return Report{}, err
	}
	return Report{Inventory: inventory, Summary: summarize(inventory), Findings: diagnose(inventory, policy)}, nil
}

// scanInventory performs doctor's single pass over every evidence kind.
func scanInventory(ctx context.Context, reader client.Reader, namespace string, now time.Time) (Inventory, error) {
	inventory := Inventory{Namespace: namespace, ObservedAt: metav1.NewTime(now.UTC().Truncate(time.Second)), Complete: true, Resources: []Resource{}, Coverage: []Observation{}}
	for _, list := range evidenceLists() {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		gvk := list.GetObjectKind().GroupVersionKind()
		observation := Observation{Kind: strings.TrimSuffix(gvk.Kind, "List"), APIVersion: gvk.GroupVersion().String(), Complete: true}
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			observation.Complete = false
			observation.Error = err.Error()
			inventory.Complete = false
		} else if err := meta.EachListItem(list, func(object runtime.Object) error {
			resource, ok := object.(client.Object)
			if !ok {
				return fmt.Errorf("unexpected inventory object %T", object)
			}
			inventory.Resources = append(inventory.Resources, project(resource, observation.APIVersion, observation.Kind))
			return nil
		}); err != nil {
			return Inventory{}, err
		}
		inventory.Coverage = append(inventory.Coverage, observation)
	}
	slices.SortFunc(inventory.Resources, func(a, b Resource) int { return strings.Compare(key(a.ObjectRef), key(b.ObjectRef)) })
	return inventory, nil
}

func validatePolicy(policy Policy) error {
	if policy.UnusedFor <= 0 || policy.UnhealthyFor <= 0 || policy.LargePVCBytes <= 0 {
		return fmt.Errorf("audit ages and requested-capacity threshold must be positive")
	}
	return nil
}

func evidenceLists() []client.ObjectList {
	lists := []struct {
		list      client.ObjectList
		api, kind string
	}{
		{&repositories.RepositoryList{}, repoAPI, "RepositoryList"},
		{&repositories.WorktreeList{}, repoAPI, "WorktreeList"},
		{&repositories.WorktreeExecList{}, repoAPI, "WorktreeExecList"},
		{&repositories.RepositoryExecList{}, repoAPI, "RepositoryExecList"},
		{&repositories.RepositorySyncList{}, repoAPI, "RepositorySyncList"},
		{&workspaces.WorkspaceList{}, workspaceAPI, "WorkspaceList"},
		{&workspaces.WorkspaceEnvironmentList{}, workspaceAPI, "WorkspaceEnvironmentList"},
		{&workspaces.WorkspaceExecList{}, workspaceAPI, "WorkspaceExecList"},
		{&corev1.PersistentVolumeClaimList{}, "v1", "PersistentVolumeClaimList"},
		{&corev1.PodList{}, "v1", "PodList"},
		{&corev1.EventList{}, "v1", "EventList"},
		{&batchv1.JobList{}, "batch/v1", "JobList"},
		{&coordinationv1.LeaseList{}, "coordination.k8s.io/v1", "LeaseList"},
	}
	result := make([]client.ObjectList, 0, len(lists))
	for _, item := range lists {
		item.list.GetObjectKind().SetGroupVersionKind(schema.FromAPIVersionAndKind(item.api, item.kind))
		result = append(result, item.list)
	}
	return result
}

func project(object client.Object, api, kind string) Resource {
	r := Resource{ObjectRef: ObjectRef{APIVersion: api, Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(), ResourceVersion: object.GetResourceVersion()}, CreatedAt: object.GetCreationTimestamp(), DeletingAt: object.GetDeletionTimestamp(), Finalizers: slices.Clone(object.GetFinalizers()), Owners: slices.Clone(object.GetOwnerReferences()), Generation: object.GetGeneration()}
	for _, owner := range r.Owners {
		r.References = append(r.References, Reference{Relation: ownerRelation, Target: ObjectRef{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: r.Namespace, Name: owner.Name, UID: owner.UID}})
	}
	projectRC(&r, object)
	projectKubernetes(&r, object)
	slices.Sort(r.Finalizers)
	return r
}

func (r *Resource) reference(api, kind, name, relation string) {
	if name == "" {
		return
	}
	r.References = append(r.References, Reference{Target: ObjectRef{APIVersion: api, Kind: kind, Namespace: r.Namespace, Name: name}, Relation: relation})
}

func projectRC(r *Resource, object client.Object) {
	switch o := object.(type) {
	case *repositories.Worktree:
		r.Conditions, r.ObservedGeneration, r.Locked = o.Status.Conditions, o.Status.ObservedGeneration, o.Spec.Lock
		r.reference(repoAPI, "Repository", o.Spec.RepositoryRef.Name, "source")
		r.reference("v1", pvcKind, cmp.Or(o.Status.VolumeClaimName, o.Name), pvcRelation)
		r.reference("batch/v1", jobKind, o.Status.JobName, "bootstrap")
	case *repositories.Repository:
		r.Conditions, r.ObservedGeneration = o.Status.Conditions, o.Status.ObservedGeneration
		r.reference("v1", pvcKind, cmp.Or(o.Status.VolumeClaimName, o.Name), pvcRelation)
	case *workspaces.Workspace:
		r.Conditions, r.ObservedGeneration, r.Phase = o.Status.Conditions, o.Status.ObservedGeneration, string(o.Spec.DesiredState)
		name := o.Status.HomeVolumeClaimName
		if o.Spec.OS != corev1.OSName("darwin") {
			name = cmp.Or(name, o.Name)
		}
		r.reference("v1", pvcKind, name, pvcRelation)
		r.reference("v1", podKind, o.Status.RuntimePodName, runtimeRelation)
		for _, mount := range o.Spec.Mounts {
			if mount.WorktreeRef != nil {
				r.reference(repoAPI, worktreeKind, mount.WorktreeRef.Name, mountRelation)
			}
			if mount.RepositoryRef != nil {
				r.reference(repoAPI, "Repository", mount.RepositoryRef.Name, mountRelation)
			}
		}
	case *workspaces.WorkspaceEnvironment:
		r.Conditions, r.ObservedGeneration = o.Status.Conditions, o.Status.ObservedGeneration
		r.reference("v1", pvcKind, o.Status.CurrentVolumeClaimName, pvcRelation)
		r.reference("v1", pvcKind, o.Status.DraftVolumeClaimName, pvcRelation)
		r.reference("v1", podKind, o.Status.EditorPodName, runtimeRelation)
	case *workspaces.WorkspaceExec:
		r.Conditions, r.ObservedGeneration = o.Status.Conditions, o.Status.ObservedGeneration
		r.Phase, r.CompletedAt, r.AttachedClients = string(o.Status.Phase), o.Status.CompletedAt, o.Status.AttachedClients
		r.Terminal = o.Status.Phase.Terminal()
		r.reference(workspaceAPI, string(o.Spec.TargetRef.Kind), o.Spec.TargetRef.Name, "execution")
	case *repositories.WorktreeExec:
		r.Conditions = o.Status.Conditions
		r.reference(repoAPI, worktreeKind, o.Spec.WorktreeRef.Name, "execution")
		r.reference("batch/v1", jobKind, o.Status.JobName, "execution-job")
		projectResult(r)
	case *repositories.RepositoryExec:
		r.Conditions = o.Status.Conditions
		r.reference(repoAPI, "Repository", o.Spec.RepositoryRef.Name, "execution")
		r.reference("batch/v1", jobKind, o.Status.JobName, "execution-job")
		projectResult(r)
	case *repositories.RepositorySync:
		r.Conditions = o.Status.Conditions
		r.reference(repoAPI, "Repository", o.Spec.RepositoryRef.Name, "execution")
		r.reference("batch/v1", jobKind, o.Status.JobName, "execution-job")
		projectResult(r)
		if o.Status.CompletedAt != nil {
			r.CompletedAt = o.Status.CompletedAt
		}
	}
}

func projectResult(r *Resource) {
	condition := meta.FindStatusCondition(r.Conditions, repositories.WorktreeExecConditionSucceeded)
	if condition == nil || (condition.Status != metav1.ConditionTrue && condition.Status != metav1.ConditionFalse) {
		return
	}
	r.Terminal = true
	r.CompletedAt = &condition.LastTransitionTime
	r.Phase = string(workspaces.WorkspaceExecPhaseSucceeded)
	if condition.Status == metav1.ConditionFalse {
		r.Phase = string(workspaces.WorkspaceExecPhaseFailed)
	}
}

func projectKubernetes(r *Resource, object client.Object) {
	switch o := object.(type) {
	case *corev1.PersistentVolumeClaim:
		r.Phase = string(o.Status.Phase)
		if q, ok := o.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			n := q.Value()
			r.RequestedBytes = &n
		}
		if q, ok := o.Status.Capacity[corev1.ResourceStorage]; ok {
			n := q.Value()
			r.CapacityBytes = &n
		}
		if o.Spec.StorageClassName != nil {
			r.StorageClass = *o.Spec.StorageClassName
		}
	case *corev1.Pod:
		r.Phase, r.Reason, r.Message = string(o.Status.Phase), o.Status.Reason, o.Status.Message
		r.Terminal = o.Status.Phase == corev1.PodSucceeded || o.Status.Phase == corev1.PodFailed
		projectVolumes(r, o.Spec.Volumes)
	case *batchv1.Job:
		for _, c := range o.Status.Conditions {
			if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
				r.Terminal, r.Phase, r.CompletedAt = true, string(c.Type), &c.LastTransitionTime
			}
		}
		projectVolumes(r, o.Spec.Template.Spec.Volumes)
	case *coordinationv1.Lease:
		if o.Spec.HolderIdentity != nil {
			r.Holder = *o.Spec.HolderIdentity
		}
		// Repository reservations are durable; expiration is never a release.
		r.Reservation = o.Annotations[repositoryaccess.StateAnnotation]
	case *corev1.Event:
		r.Reason, r.Phase, r.Message = o.Reason, o.Type, o.Message
		r.References = append(r.References, Reference{Relation: "event", Target: ObjectRef{APIVersion: o.InvolvedObject.APIVersion, Kind: o.InvolvedObject.Kind, Namespace: o.InvolvedObject.Namespace, Name: o.InvolvedObject.Name, UID: o.InvolvedObject.UID}})
	}
}

func projectVolumes(r *Resource, volumes []corev1.Volume) {
	for _, volume := range volumes {
		if volume.PersistentVolumeClaim != nil {
			r.reference("v1", pvcKind, volume.PersistentVolumeClaim.ClaimName, mountRelation)
		}
	}
}

func key(ref ObjectRef) string {
	return ref.APIVersion + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

func matches(ref, target ObjectRef) bool {
	return key(ref) == key(target) && (ref.UID == "" || ref.UID == target.UID)
}
