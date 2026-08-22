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

package repositories

import (
	"context"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
)

const (
	repositoryBootstrapJobSuffix = "-bootstrap-"
	repositoryCredentialRoot     = "/run/rc/credentials"
	repositoryManagedByLabel     = "app.kubernetes.io/managed-by"
	repositoryManagedByValue     = "rc"
	repositoryNameLabel          = "rc.ayaka.io/repository"
)

// RepositoryReconciler reconciles a Repository object.
type RepositoryReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	RunnerImage string
}

// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=configs.rc.ayaka.io,resources=credentials,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile ensures that every Repository owns one persistent parent volume and
// that its configured remote is bootstrapped into that volume.
//
//nolint:gocyclo // Reconcile is an existing explicit resource lifecycle state machine.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	repository := new(repositoriesv1alpha1.Repository)
	if err := r.Get(ctx, req.NamespacedName, repository); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	claim := new(corev1.PersistentVolumeClaim)
	claimKey := types.NamespacedName{Name: repository.Name, Namespace: repository.Namespace}
	err := r.Get(ctx, claimKey, claim)
	if errors.IsNotFound(err) {
		claim = parentVolumeClaim(repository)

		err := controllerutil.SetControllerReference(repository, claim, r.Scheme)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("set Repository owner on PersistentVolumeClaim: %w", err)
		}

		err = r.Create(ctx, claim)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create parent PersistentVolumeClaim: %w", err)
		}

		log.Info("Created parent PersistentVolumeClaim", "name", claim.Name)
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "Provisioning", "Parent volume is provisioning", claim.Name, nil)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get parent PersistentVolumeClaim: %w", err)
	}
	if !metav1.IsControlledBy(claim, repository) {
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "VolumeClaimConflict", "A PersistentVolumeClaim with the Repository name already exists and is not owned by this Repository", "", nil)
	}

	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != repository.Spec.Storage.StorageClassName {
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "StorageClassChangeUnsupported", "Changing storageClassName on an existing Repository is not supported", claim.Name, nil)
	}

	currentSize := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	desiredSize := repository.Spec.Storage.Size

	switch desiredSize.Cmp(currentSize) {
	case -1:
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "VolumeShrinkUnsupported", "Shrinking a Repository volume is not supported", claim.Name, nil)
	case 1:
		claim.Spec.Resources.Requests[corev1.ResourceStorage] = desiredSize

		err := r.Update(ctx, claim)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("expand parent PersistentVolumeClaim: %w", err)
		}

		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "Expanding", "Parent volume is expanding", claim.Name, nil)
	}

	if ready := meta.FindStatusCondition(repository.Status.Conditions, repositoriesv1alpha1.RepositoryConditionStorageReady); ready != nil &&
		ready.Status == metav1.ConditionTrue && repository.Status.ObservedGeneration == repository.Generation &&
		claim.Status.Phase == corev1.ClaimBound {
		// Re-read a completed Job so an upgraded controller can backfill the
		// timestamp and a future sync Job can advance it without duplicate work.
		completedJob := new(batchv1.Job)
		jobKey := types.NamespacedName{Name: repositoryBootstrapJobName(repository), Namespace: repository.Namespace}

		err := r.Get(ctx, jobKey, completedJob)
		if err == nil {
			if completionTime := completedJob.Status.CompletionTime; completionTime != nil {
				if condition := jobCondition(completedJob, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
					return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionTrue, "RepositoryReady", "Repository Git content is ready", claim.Name, completionTime)
				}
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get completed Repository bootstrap Job: %w", err)
		}

		return ctrl.Result{}, nil
	}

	job := new(batchv1.Job)
	jobKey := types.NamespacedName{Name: repositoryBootstrapJobName(repository), Namespace: repository.Namespace}

	err = r.Get(ctx, jobKey, job)
	if errors.IsNotFound(err) {
		credential, credentialErr := r.repositoryCredential(ctx, repository)
		if credentialErr != nil {
			if errors.IsNotFound(credentialErr) {
				return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "CredentialNotFound", "Referenced Credential does not exist", claim.Name, nil)
			}

			return ctrl.Result{}, credentialErr
		}

		job = repositoryBootstrapJob(repository, r.RunnerImage, credential)
		if err := controllerutil.SetControllerReference(repository, job, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set Repository owner on bootstrap Job: %w", err)
		}
		if err := r.Create(ctx, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("create Repository bootstrap Job: %w", err)
		}

		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "Initializing", "Repository bootstrap Job is running", claim.Name, nil)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get Repository bootstrap Job: %w", err)
	}

	if !metav1.IsControlledBy(job, repository) {
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "BootstrapJobConflict", "A bootstrap Job with the expected name is not owned by this Repository", claim.Name, nil)
	}
	if condition := jobCondition(job, batchv1.JobFailed); condition != nil && condition.Status == corev1.ConditionTrue {
		message := condition.Message
		if message == "" {
			message = "Repository bootstrap Job failed"
		}
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "BootstrapFailed", message, claim.Name, nil)
	}
	if condition := jobCondition(job, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
		return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionTrue, "RepositoryReady", "Repository Git content is ready", claim.Name, job.Status.CompletionTime)
	}

	return ctrl.Result{}, r.setStorageReady(ctx, repository, metav1.ConditionFalse, "Initializing", "Repository bootstrap Job is running", claim.Name, nil)
}

func (r *RepositoryReconciler) repositoryCredential(ctx context.Context, repository *repositoriesv1alpha1.Repository) (*configsv1alpha1.Credential, error) {
	if repository.Spec.Remote.CredentialRef == nil {
		return nil, nil
	}

	credential := new(configsv1alpha1.Credential)
	key := types.NamespacedName{
		Name:      repository.Spec.Remote.CredentialRef.Name,
		Namespace: repository.Namespace,
	}
	if err := r.Get(ctx, key, credential); err != nil {
		return nil, err
	}

	return credential, nil
}

func parentVolumeClaim(repository *repositoriesv1alpha1.Repository) *corev1.PersistentVolumeClaim {
	filesystem := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repository.Name,
			Namespace: repository.Namespace,
			Labels: map[string]string{
				repositoryManagedByLabel: repositoryManagedByValue,
				repositoryNameLabel:      repository.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			// TODO(repository-storage-options): Configurable access modes and
			// volume mode are deferred until a non-filesystem or multi-writer
			// Repository consumer is owner-approved.
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &repository.Spec.Storage.StorageClassName,
			VolumeMode:       &filesystem,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: repository.Spec.Storage.Size},
			},
		},
	}
}

func (r *RepositoryReconciler) setStorageReady(
	ctx context.Context,
	repository *repositoriesv1alpha1.Repository,
	status metav1.ConditionStatus,
	reason string,
	message string,
	claimName string,
	lastUpdatedAt *metav1.Time,
) error {
	key := client.ObjectKeyFromObject(repository)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := new(repositoriesv1alpha1.Repository)

		err := r.Get(ctx, key, current)
		if err != nil {
			return client.IgnoreNotFound(err)
		}

		before := current.DeepCopy()
		current.Status.ObservedGeneration = current.Generation
		current.Status.VolumeClaimName = claimName
		if lastUpdatedAt != nil {
			current.Status.LastUpdatedAt = lastUpdatedAt.DeepCopy()
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type:               repositoriesv1alpha1.RepositoryConditionStorageReady,
			Status:             status,
			ObservedGeneration: current.Generation,
			Reason:             reason,
			Message:            message,
		})
		if current.Status.ObservedGeneration == before.Status.ObservedGeneration &&
			current.Status.VolumeClaimName == before.Status.VolumeClaimName &&
			equality.Semantic.DeepEqual(current.Status.LastUpdatedAt, before.Status.LastUpdatedAt) &&
			conditionsEqual(current.Status.Conditions, before.Status.Conditions) {
			return nil
		}

		err = r.Status().Patch(ctx, current, client.MergeFrom(before))
		if err != nil {
			return fmt.Errorf("patch Repository status: %w", err)
		}
		return nil
	})
}

func repositoryBootstrapJobName(repository *repositoriesv1alpha1.Repository) string {
	return repository.Name + repositoryBootstrapJobSuffix + strconv.FormatInt(repository.Generation, 10)
}

func repositoryBootstrapJob(
	repository *repositoriesv1alpha1.Repository,
	runnerImage string,
	credential *configsv1alpha1.Credential,
) *batchv1.Job {
	auth := repositoryBootstrapAuth(credential)
	args := make([]string, 0, 7+len(auth.headerArgs))
	args = append(args,
		"-ceu",
		repositoryBootstrapScript,
		"repository-bootstrap",
		repository.Spec.Remote.URL,
		repository.Spec.Ref,
		auth.mode,
		strconv.Itoa(len(auth.headerArgs)/2),
	)
	args = append(args, auth.headerArgs...)
	backoffLimit := int32(0)
	runAsNonRoot := true
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	allowPrivilegeEscalation := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      repositoryBootstrapJobName(repository),
			Namespace: repository.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "rc",
				"rc.ayaka.io/repository":       repository.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"rc.ayaka.io/repository": repository.Name}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsGroup,
						FSGroup:      &runAsGroup,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:         "bootstrap",
						Image:        runnerImage,
						Command:      []string{"sh"},
						Args:         args,
						WorkingDir:   "/repository",
						Env:          append([]corev1.EnvVar{{Name: "HOME", Value: "/tmp"}}, auth.env...),
						VolumeMounts: append([]corev1.VolumeMount{{Name: workerVolumeName, MountPath: workerMountPath}}, auth.mounts...),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{allCapabilitiesDrop}},
						},
					}},
					Volumes: append([]corev1.Volume{{
						Name: workerVolumeName,
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: repository.Name,
						}},
					}}, auth.volumes...),
				},
			},
		},
	}
}

type repositoryBootstrapAuthentication struct {
	mode       string
	env        []corev1.EnvVar
	volumes    []corev1.Volume
	mounts     []corev1.VolumeMount
	headerArgs []string
}

func repositoryBootstrapAuth(credential *configsv1alpha1.Credential) repositoryBootstrapAuthentication {
	if credential == nil {
		return repositoryBootstrapAuthentication{mode: "none"}
	}

	auth := repositoryBootstrapAuthentication{}
	secretMode := int32(0400)
	addSecret := func(purpose string, reference configsv1alpha1.SecretKeyReference) string {
		volumeName := "credential-" + purpose
		path := repositoryCredentialRoot + "/" + purpose
		auth.volumes = append(auth.volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  reference.Name,
				Items:       []corev1.KeyToPath{{Key: reference.Key, Path: "value"}},
				DefaultMode: &secretMode,
			}},
		})
		auth.mounts = append(auth.mounts, corev1.VolumeMount{
			Name: volumeName, MountPath: path, SubPath: "value", ReadOnly: true,
		})

		return path
	}

	switch credential.Spec.Type {
	case configsv1alpha1.CredentialTypeSSHPrivateKey:
		auth.mode = "ssh"
		privateKeyPath := addSecret("ssh-private-key", credential.Spec.SSHPrivateKey.PrivateKeyRef)
		knownHostsPath := addSecret("ssh-known-hosts", credential.Spec.SSHPrivateKey.KnownHostsRef)
		auth.env = []corev1.EnvVar{{Name: "GIT_SSH_COMMAND", Value: "ssh -i " + privateKeyPath + " -o UserKnownHostsFile=" + knownHostsPath + " -o IdentitiesOnly=yes"}}
	case configsv1alpha1.CredentialTypeHTTPBasicAuth:
		auth.mode = "basic"
		passwordPath := addSecret("http-password", credential.Spec.HTTPBasicAuth.PasswordRef)
		auth.env = []corev1.EnvVar{{Name: "RC_GIT_USERNAME", Value: credential.Spec.HTTPBasicAuth.Username}, {Name: "RC_GIT_PASSWORD_FILE", Value: passwordPath}}
	case configsv1alpha1.CredentialTypeHTTPBearerToken:
		auth.mode = "bearer"
		tokenPath := addSecret("http-token", credential.Spec.HTTPBearerToken.TokenRef)
		auth.env = []corev1.EnvVar{{Name: "RC_GIT_TOKEN_FILE", Value: tokenPath}}
	case configsv1alpha1.CredentialTypeHTTPHeaders:
		auth.mode = "headers"
		for index, header := range credential.Spec.HTTPHeaders.Headers {
			path := addSecret("http-header-"+strconv.Itoa(index), header.ValueRef)
			auth.headerArgs = append(auth.headerArgs, header.Name, path)
		}
	default:
		auth.mode = "unsupported"
	}

	return auth
}

func jobCondition(job *batchv1.Job, conditionType batchv1.JobConditionType) *batchv1.JobCondition {
	for index := range job.Status.Conditions {
		condition := &job.Status.Conditions[index]
		if condition.Type == conditionType {
			return condition
		}
	}

	return nil
}

const repositoryBootstrapScript = `
remote="$1"
ref="$2"
auth_mode="$3"
header_count="$4"
shift 4

case "$auth_mode" in
none|ssh)
  ;;
basic)
  export GIT_CONFIG_COUNT=1
  export GIT_CONFIG_KEY_0=credential.helper
  export GIT_CONFIG_VALUE_0='!f() { printf "username=%s\\npassword=%s\\n" "$RC_GIT_USERNAME" "$(cat "$RC_GIT_PASSWORD_FILE")"; }; f'
  ;;
bearer)
  token="$(cat "$RC_GIT_TOKEN_FILE")"
  export GIT_CONFIG_COUNT=1
  export GIT_CONFIG_KEY_0=http.extraHeader
  export GIT_CONFIG_VALUE_0="Authorization: Bearer $token"
  ;;
headers)
  index=0
  while [ "$index" -lt "$header_count" ]; do
    header_name="$1"
    header_file="$2"
    shift 2
    header_value="$(cat "$header_file")"
    export "GIT_CONFIG_KEY_$index=http.extraHeader"
    export "GIT_CONFIG_VALUE_$index=$header_name: $header_value"
    index=$((index + 1))
  done
  export GIT_CONFIG_COUNT="$header_count"
  ;;
*)
  echo "unsupported Repository Credential type" >&2
  exit 64
  ;;
esac

git config --global --add safe.directory /repository
git -C /repository init
if git -C /repository remote get-url origin >/dev/null 2>&1; then
  git -C /repository remote set-url origin "$remote"
else
  git -C /repository remote add origin "$remote"
fi
git -C /repository fetch --prune origin
git -C /repository remote set-head origin -a >/dev/null 2>&1 || true

if [ -n "$ref" ]; then
  case "$ref" in
  refs/heads/*)
    target="refs/remotes/origin/${ref#refs/heads/}"
    ;;
  refs/tags/*)
    target="$ref"
    ;;
  refs/*)
    git -C /repository fetch origin "$ref"
    target=FETCH_HEAD
    ;;
  *)
    target="$ref"
    ;;
  esac
else
  target="$(git -C /repository symbolic-ref --quiet refs/remotes/origin/HEAD || git -C /repository rev-parse FETCH_HEAD)"
fi

git -C /repository reset --hard "$target"
git -C /repository clean -ffdx
git -C /repository rev-parse --verify HEAD
`

func conditionsEqual(left, right []metav1.Condition) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}

	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.RunnerImage == "" {
		return fmt.Errorf("repository runner image must not be empty")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&repositoriesv1alpha1.Repository{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Named("repositories-repository").
		Complete(r)
}
