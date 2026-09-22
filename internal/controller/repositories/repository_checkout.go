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
	"strconv"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	"github.com/nekomeowww/rc/internal/runtimepolicy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	repositoryCheckoutContainer      = "bootstrap"
	repositoryGitSSHCommand          = "GIT_SSH_COMMAND"
	repositoryCredentialRoot         = "/run/rc/credentials"
	repositorySubmoduleModeNone      = "none"
	repositorySubmoduleModeDirect    = "direct"
	repositorySubmoduleModeRecursive = "recursive"
)

// repositoryCredential resolves only the credential selected by the Repository.
func repositoryCredential(ctx context.Context, reader client.Reader, repository *repositoriesv1alpha1.Repository) (*configsv1alpha1.Credential, error) {
	if repository.Spec.Remote.CredentialRef == nil {
		return nil, nil
	}

	credential := new(configsv1alpha1.Credential)
	key := types.NamespacedName{
		Name:      repository.Spec.Remote.CredentialRef.Name,
		Namespace: repository.Namespace,
	}
	if err := reader.Get(ctx, key, credential); err != nil {
		return nil, err
	}

	return credential, nil
}

// repositoryCheckoutJob builds the authenticated fetch/reset operation used by
// bootstrap and sync. Its checkout container reports the commit through the
// termination message after Git and submodule operations succeed.
func repositoryCheckoutJob(
	repository *repositoriesv1alpha1.Repository,
	jobName, runnerImage string,
	credential *configsv1alpha1.Credential,
) *batchv1.Job {
	auth := repositoryCheckoutAuth(credential)
	args := make([]string, 0, 8+len(auth.headerArgs))
	args = append(args,
		"-ceu",
		repositoryCheckoutScript,
		"repository-checkout",
		repository.Spec.Remote.URL,
		repository.Spec.Ref,
		repositorySubmoduleMode(repository),
		auth.mode,
		strconv.Itoa(len(auth.headerArgs)/2),
	)
	args = append(args, auth.headerArgs...)
	backoffLimit := int32(0)
	allowPrivilegeEscalation := false
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
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
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: runtimepolicy.AgentPodSecurityContext(),
					Containers: []corev1.Container{{
						Name:         repositoryCheckoutContainer,
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

type repositoryCheckoutAuthentication struct {
	mode       string
	env        []corev1.EnvVar
	volumes    []corev1.Volume
	mounts     []corev1.VolumeMount
	headerArgs []string
}

func repositoryCheckoutAuth(credential *configsv1alpha1.Credential) repositoryCheckoutAuthentication {
	if credential == nil {
		return repositoryCheckoutAuthentication{mode: "none"}
	}

	auth := repositoryCheckoutAuthentication{}
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
		auth.env = []corev1.EnvVar{{Name: repositoryGitSSHCommand, Value: "ssh -i " + privateKeyPath + " -o UserKnownHostsFile=" + knownHostsPath + " -o IdentitiesOnly=yes"}}
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

func repositorySubmoduleMode(repository *repositoriesv1alpha1.Repository) string {
	if repository.Spec.Submodules == nil {
		return repositorySubmoduleModeNone
	}
	if repository.Spec.Submodules.Recursive {
		return repositorySubmoduleModeRecursive
	}

	return repositorySubmoduleModeDirect
}

const repositoryCheckoutScript = `
remote="$1"
ref="$2"
submodule_mode="$3"
auth_mode="$4"
header_count="$5"
shift 5

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
git -C /repository config checkout.workers 8
git -C /repository config checkout.thresholdForParallelism 100
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
case "$submodule_mode" in
none)
  ;;
direct)
  git -C /repository submodule sync
  git -C /repository submodule update --init
  ;;
recursive)
  git -C /repository submodule sync --recursive
  git -C /repository submodule update --init --recursive
  ;;
*)
  echo "unsupported Repository submodule mode" >&2
  exit 64
  ;;
esac
commit="$(git -C /repository rev-parse --verify HEAD)"
printf '%s\n' "$commit"
printf '%s\n' "$commit" > /dev/termination-log
`
