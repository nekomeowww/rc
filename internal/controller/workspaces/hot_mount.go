package workspaces

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	hotMountHelperLabel          = "workspaces.rc.ayaka.io/hot-mount-helper"
	hotMountCleanupLabel         = "workspaces.rc.ayaka.io/hot-mount-cleanup"
	hotMountSignatureAnnotation  = "workspaces.rc.ayaka.io/hot-mount-signature"
	hotMountRuntimeUIDAnnotation = "workspaces.rc.ayaka.io/hot-mount-runtime-uid"
	hotMountHostBase             = "/var/lib/rc/hot-mounts"
	hotMountLabelValue           = "true"
	hotMountShell                = "/bin/sh"
	hotMountVisibleEnv           = "HOT_VISIBLE"
	hotMountRootEnv              = "HOT_ROOT"
	hotMountVisibleVolume        = "visible"
	hotMountRootsVolume          = "roots"
)

const hotMountScript = `set -eu
visible_mounted=false
root_mounted=false
cleanup() {
  status=$?
  trap - EXIT
  if [ "$visible_mounted" = true ]; then
    umount "$HOT_VISIBLE" || status=1
  fi
  if [ "$root_mounted" = true ]; then
    umount "$HOT_ROOT" || status=1
  fi
  rmdir "$HOT_VISIBLE" 2>/dev/null || :
  if [ -n "$HOT_SUBPATH" ]; then
    rmdir "$HOT_ROOT" 2>/dev/null || :
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 0' TERM INT
if mountpoint -q "$HOT_VISIBLE"; then
  echo 'Hot Worktree target is already mounted' >&2
  exit 1
fi
mkdir -p -- "$HOT_VISIBLE"
if [ -n "$HOT_SUBPATH" ]; then
  if mountpoint -q "$HOT_ROOT"; then
    echo 'Hot Worktree metadata target is already mounted' >&2
    exit 1
  fi
  mkdir -p -- "$HOT_ROOT"
  mount --bind /source "$HOT_ROOT"
  root_mounted=true
  source_path="/source/$HOT_SUBPATH"
else
  source_path=/source
fi
mount --bind "$source_path" "$HOT_VISIBLE"
visible_mounted=true
if [ "$HOT_READ_ONLY" = true ]; then
  mount -o remount,bind,ro "$HOT_VISIBLE"
fi
while :; do sleep 3600 & wait "$!"; done
`

const hotMountCleanupScript = `set -eu
if mountpoint -q "$HOT_VISIBLE"; then
  umount "$HOT_VISIBLE"
fi
if mountpoint -q "$HOT_ROOT"; then
  umount "$HOT_ROOT"
fi
rmdir "$HOT_VISIBLE" 2>/dev/null || :
rmdir "$HOT_ROOT" 2>/dev/null || :
`

func hotMountHostRoot(workspace *workspacesv1alpha1.Workspace) string {
	return path.Join(hotMountHostBase, workspace.Namespace, string(workspace.UID))
}

func validateHotMountPaths(mounts []workspacesv1alpha1.WorkspaceMount) (string, string) {
	for index, mount := range mounts {
		if mount.Name == "hot-worktrees" || mount.Name == "hot-worktree-roots" {
			return "ReservedMountName", fmt.Sprintf("Mount name %s is reserved for hot Worktree mounts", mount.Name)
		}
		if mount.WorktreeRef == nil {
			continue
		}
		if path.Clean(mount.Path) != mount.Path {
			return "InvalidHotMountPath", fmt.Sprintf("Hot Worktree mount %s has a non-canonical path", mount.Name)
		}
		for otherIndex, other := range mounts {
			if index == otherIndex {
				continue
			}
			if mount.Path == other.Path || strings.HasPrefix(mount.Path, other.Path+"/") || strings.HasPrefix(other.Path, mount.Path+"/") {
				return "OverlappingMountPaths", fmt.Sprintf("Mount paths %s and %s overlap", mount.Path, other.Path)
			}
		}
	}
	return "", ""
}

func hotMountHelperName(workspace *workspacesv1alpha1.Workspace, mount hotWorktreeMount) string {
	sum := sha256.Sum256([]byte(string(workspace.UID) + "/" + mount.name))
	suffix := "-hot-" + hex.EncodeToString(sum[:4])
	name := strings.TrimRight(workspace.Name[:min(len(workspace.Name), 57-len(suffix))], "-")
	return name + suffix
}

func hotMountCleanupPod(workspace *workspacesv1alpha1.Workspace, helper *corev1.Pod, image string) (*corev1.Pod, error) {
	if helper.Spec.NodeName == "" || len(helper.Spec.Containers) == 0 {
		return nil, fmt.Errorf("hot Worktree mount Pod %s has no node or container", helper.Name)
	}
	values := map[string]string{}
	for _, env := range helper.Spec.Containers[0].Env {
		values[env.Name] = env.Value
	}
	root := hotMountHostRoot(workspace)
	visible := values[hotMountVisibleEnv]
	metadata := values[hotMountRootEnv]
	if !strings.HasPrefix(visible, "/publish-visible/") || !strings.HasPrefix(metadata, "/publish-roots/") {
		return nil, fmt.Errorf("hot Worktree mount Pod %s has invalid cleanup paths", helper.Name)
	}
	name := helper.Name + "-clean"
	kind := corev1.HostPathDirectoryOrCreate
	propagation := corev1.MountPropagationBidirectional
	privileged, automountToken := true, false
	runAsRoot := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: workspace.Namespace,
			Labels: map[string]string{workspaceManagedByLabel: workspace.Name, hotMountCleanupLabel: hotMountLabelValue},
		},
		Spec: corev1.PodSpec{
			NodeName: helper.Spec.NodeName, RestartPolicy: corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automountToken,
			Containers: []corev1.Container{{
				Name: "cleaner", Image: image, Command: []string{hotMountShell, "-c", hotMountCleanupScript},
				Env: []corev1.EnvVar{{Name: hotMountVisibleEnv, Value: visible}, {Name: hotMountRootEnv, Value: metadata}},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged, RunAsUser: &runAsRoot,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: hotMountVisibleVolume, MountPath: "/publish-visible", MountPropagation: &propagation},
					{Name: hotMountRootsVolume, MountPath: "/publish-roots", MountPropagation: &propagation},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: hotMountVisibleVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path.Join(root, hotMountVisibleVolume), Type: &kind}}},
				{Name: hotMountRootsVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path.Join(root, hotMountRootsVolume), Type: &kind}}},
			},
		},
	}, nil
}

func (r *WorkspaceReconciler) cleanFailedHotMount(ctx context.Context, workspace *workspacesv1alpha1.Workspace, helper *corev1.Pod) (bool, string, error) {
	cleaner := new(corev1.Pod)
	key := client.ObjectKey{Name: helper.Name + "-clean", Namespace: workspace.Namespace}
	if err := r.APIReader.Get(ctx, key, cleaner); errors.IsNotFound(err) {
		cleaner, err = hotMountCleanupPod(workspace, helper, r.RunnerImage)
		if err != nil {
			return false, "", err
		}
		if err := controllerutil.SetControllerReference(workspace, cleaner, r.Scheme); err != nil {
			return false, "", fmt.Errorf("set Workspace owner on hot Worktree cleanup Pod: %w", err)
		}
		if err := r.Create(ctx, cleaner); err != nil {
			return false, "", fmt.Errorf("create hot Worktree cleanup Pod: %w", err)
		}
		return false, "Cleaning up failed hot Worktree mount", nil
	} else if err != nil {
		return false, "", fmt.Errorf("get hot Worktree cleanup Pod: %w", err)
	}
	if !metav1.IsControlledBy(cleaner, workspace) {
		return false, "", fmt.Errorf("hot Worktree cleanup Pod %s is not owned by this Workspace", cleaner.Name)
	}
	if cleaner.Status.Phase == corev1.PodFailed {
		return false, "Hot Worktree mount cleanup failed", nil
	}
	if cleaner.Status.Phase != corev1.PodSucceeded {
		return false, "Cleaning up failed hot Worktree mount", nil
	}
	return true, "", nil
}

func (r *WorkspaceReconciler) ensureFailedHotMountClean(ctx context.Context, workspace *workspacesv1alpha1.Workspace, helper *corev1.Pod) (bool, string, error) {
	if helper.Status.Phase != corev1.PodFailed && helper.Status.Phase != corev1.PodSucceeded {
		return true, "", nil
	}
	return r.cleanFailedHotMount(ctx, workspace, helper)
}

func hotMountSignature(mount hotWorktreeMount) string {
	encoded := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%t", mount.name, mount.worktree, mount.path, mount.claimName, mount.subPath, mount.readOnly)
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:])
}

func hotMountHelperPod(workspace *workspacesv1alpha1.Workspace, runtime *corev1.Pod, mount hotWorktreeMount, image string) (*corev1.Pod, error) {
	if runtime.Spec.NodeName == "" {
		return nil, fmt.Errorf("workspace runtime node is unknown")
	}
	if mount.path == "." || path.IsAbs(mount.path) || strings.HasPrefix(mount.path, "../") {
		return nil, fmt.Errorf("invalid hot Worktree mount path %q", mount.path)
	}
	if mount.subPath != "" && (path.IsAbs(mount.subPath) || path.Clean(mount.subPath) != mount.subPath || strings.HasPrefix(mount.subPath, "../")) {
		return nil, fmt.Errorf("invalid hot Worktree source path %q", mount.subPath)
	}
	signature := hotMountSignature(mount)
	root := hotMountHostRoot(workspace)
	kind := corev1.HostPathDirectoryOrCreate
	propagation := corev1.MountPropagationBidirectional
	privileged, allowPrivilegeEscalation, runAsNonRoot := true, true, false
	automountToken := false
	runAsRoot := int64(0)
	readOnly := fmt.Sprint(mount.readOnly)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: hotMountHelperName(workspace, mount), Namespace: workspace.Namespace,
			Labels:      map[string]string{workspaceManagedByLabel: workspace.Name, hotMountHelperLabel: hotMountLabelValue},
			Annotations: map[string]string{hotMountSignatureAnnotation: signature, hotMountRuntimeUIDAnnotation: string(runtime.UID)},
		},
		Spec: corev1.PodSpec{
			NodeName: runtime.Spec.NodeName, RestartPolicy: corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &automountToken,
			TerminationGracePeriodSeconds: func() *int64 { value := int64(60); return &value }(),
			Containers: []corev1.Container{{
				Name: "mounter", Image: image, Command: []string{hotMountShell, "-c", hotMountScript},
				Env: []corev1.EnvVar{
					{Name: hotMountVisibleEnv, Value: path.Join("/publish-visible", mount.path)},
					{Name: hotMountRootEnv, Value: path.Join("/publish-roots", mount.worktree)},
					{Name: "HOT_SUBPATH", Value: mount.subPath},
					{Name: "HOT_READ_ONLY", Value: readOnly},
				},
				SecurityContext: &corev1.SecurityContext{
					Privileged: &privileged, AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					RunAsUser: &runAsRoot, RunAsNonRoot: &runAsNonRoot,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
				},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
					Command: []string{"/usr/bin/mountpoint", "-q", path.Join("/publish-visible", mount.path)},
				}}, PeriodSeconds: 2, TimeoutSeconds: 2},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "worktree", MountPath: "/source", ReadOnly: mount.readOnly},
					{Name: hotMountVisibleVolume, MountPath: "/publish-visible", MountPropagation: &propagation},
					{Name: hotMountRootsVolume, MountPath: "/publish-roots", MountPropagation: &propagation},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "worktree", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: mount.claimName, ReadOnly: mount.readOnly}}},
				{Name: hotMountVisibleVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path.Join(root, hotMountVisibleVolume), Type: &kind}}},
				{Name: hotMountRootsVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path.Join(root, hotMountRootsVolume), Type: &kind}}},
			},
		},
	}, nil
}

func (r *WorkspaceReconciler) removeOrphanHotMountCleanups(ctx context.Context, workspace *workspacesv1alpha1.Workspace, helpers *corev1.PodList) (bool, string, error) {
	cleanupPods := new(corev1.PodList)
	if err := r.APIReader.List(ctx, cleanupPods, client.InNamespace(workspace.Namespace), client.MatchingLabels{
		workspaceManagedByLabel: workspace.Name, hotMountCleanupLabel: hotMountLabelValue,
	}); err != nil {
		return false, "", fmt.Errorf("list hot Worktree cleanup Pods: %w", err)
	}
	for index := range cleanupPods.Items {
		cleaner := &cleanupPods.Items[index]
		if !metav1.IsControlledBy(cleaner, workspace) {
			return false, fmt.Sprintf("Hot mount cleanup Pod %s is not owned by this Workspace", cleaner.Name), nil
		}
		helperName := strings.TrimSuffix(cleaner.Name, "-clean")
		found := false
		for helperIndex := range helpers.Items {
			if helpers.Items[helperIndex].Name == helperName {
				found = true
				break
			}
		}
		if !found {
			if cleaner.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, cleaner); err != nil && !errors.IsNotFound(err) {
					return false, "", fmt.Errorf("delete hot Worktree cleanup Pod: %w", err)
				}
			}
			return false, "Hot Worktree cleanup Pod is stopping", nil
		}
	}
	return true, "", nil
}

func (r *WorkspaceReconciler) reconcileHotMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace, runtime *corev1.Pod, desired []hotWorktreeMount, active bool) (bool, string, string, error) {
	listed := new(corev1.PodList)
	if err := r.APIReader.List(ctx, listed, client.InNamespace(workspace.Namespace), client.MatchingLabels{
		workspaceManagedByLabel: workspace.Name, hotMountHelperLabel: hotMountLabelValue,
	}); err != nil {
		return false, "", "", fmt.Errorf("list hot Worktree mount Pods: %w", err)
	}
	clean, message, err := r.removeOrphanHotMountCleanups(ctx, workspace, listed)
	if err != nil {
		return false, "", "", err
	}
	if !clean {
		return false, "Unmounting", message, nil
	}
	wanted := make(map[string]hotWorktreeMount, len(desired))
	for _, mount := range desired {
		wanted[hotMountHelperName(workspace, mount)] = mount
	}
	current := make(map[string]*corev1.Pod, len(listed.Items))
	for index := range listed.Items {
		pod := &listed.Items[index]
		if !metav1.IsControlledBy(pod, workspace) {
			return false, "HotMountConflict", fmt.Sprintf("Hot mount Pod %s is not owned by this Workspace", pod.Name), nil
		}
		mount, exists := wanted[pod.Name]
		signature := hotMountSignature(mount)
		cleaned, message, err := r.ensureFailedHotMountClean(ctx, workspace, pod)
		if err != nil {
			return false, "", "", err
		}
		if !cleaned {
			return false, "HotMountCleanup", message, nil
		}
		if !exists || pod.Annotations[hotMountSignatureAnnotation] != signature || runtime == nil || pod.Annotations[hotMountRuntimeUIDAnnotation] != string(runtime.UID) {
			if active {
				return false, "HotUnmountBlocked", "Stop active Workspace processes before removing a Worktree mount", nil
			}
			if pod.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
					return false, "", "", fmt.Errorf("delete hot Worktree mount Pod: %w", err)
				}
			}
			return false, "Unmounting", "Hot Worktree mount is stopping", nil
		}
		current[pod.Name] = pod
	}
	if runtime == nil && len(desired) == 0 {
		return true, "", "", nil
	}
	if runtime == nil || !podReady(runtime) {
		return false, reasonStarting, "Workspace runtime Pod is starting", nil
	}
	for _, mount := range desired {
		name := hotMountHelperName(workspace, mount)
		pod, exists := current[name]
		if !exists {
			pod, err := hotMountHelperPod(workspace, runtime, mount, r.RunnerImage)
			if err != nil {
				return false, "", "", err
			}
			if err := controllerutil.SetControllerReference(workspace, pod, r.Scheme); err != nil {
				return false, "", "", fmt.Errorf("set Workspace owner on hot Worktree mount Pod: %w", err)
			}
			if err := r.Create(ctx, pod); err != nil {
				return false, "", "", fmt.Errorf("create hot Worktree mount Pod: %w", err)
			}
			return false, "Mounting", fmt.Sprintf("Hot Worktree mount %s is starting", mount.name), nil
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return false, "HotMountFailed", fmt.Sprintf("Hot Worktree mount %s exited", mount.name), nil
		}
		if !podReady(pod) {
			return false, "Mounting", fmt.Sprintf("Hot Worktree mount %s is starting", mount.name), nil
		}
	}
	return true, "", "", nil
}

func (r *WorkspaceReconciler) removeHotMounts(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (bool, error) {
	ready, _, _, err := r.reconcileHotMounts(ctx, workspace, nil, nil, false)
	return ready, err
}
