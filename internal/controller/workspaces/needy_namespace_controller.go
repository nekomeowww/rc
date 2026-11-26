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

package workspaces

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/needy"
)

const (
	needyDaemonContainer = "daemon"
	// needyDaemonUser runs the daemon and the actor runner. It is the user of
	// the needy image.
	needyDaemonUser     = int64(1000)
	needyStateClaimSize = "2Gi"
	needyDaemonPoll     = 5 * time.Second
)

// Volumes of the daemon and actor runner Pods.
const (
	needyStateVolume   = "state"
	needyDataVolume    = "data"
	needyRuntimeVolume = "runtime"
	needyFilesVolume   = "files"
	needyControlPort   = "control"
	needyGatewayPort   = "gateway"
	needyAppName       = "needy"
)

// needy's directories in the daemon and actor runner Pods. The state holds
// state.db, trace.db, and run records; the data holds the base VM image and
// guest disks; the runtime holds the management socket.
const (
	needyStateDirectory   = "/var/lib/needy"
	needyDataDirectory    = "/var/lib/needy-data"
	needyRuntimeDirectory = "/run/needy"
	needyFilesDirectory   = "/etc/needy/files"
)

// NeedyReconciler runs needy in each namespace that uses it: one daemon and
// its static runners and clients files. The runners file names every
// executor Workspace; the clients file names the manager, whose token the
// manager generates before the daemon starts. The daemon scales to zero
// while no executor Workspace and no live Actor remain; its state volume
// stays.
type NeedyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config needy.Config
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles one namespace; the request name is always needy.DaemonName.
func (r *NeedyReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if !r.Config.Enabled() {
		return ctrl.Result{}, nil
	}
	namespace := request.Namespace
	workspaces, live, err := r.needyUsers(ctx, namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	var deployment appsv1.Deployment
	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: needy.DaemonName}, &deployment)
	if apierrors.IsNotFound(err) && len(workspaces) == 0 && !live {
		return ctrl.Result{}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get needy daemon Deployment: %w", err)
	}

	runners := make([]needy.StaticRunner, 0, len(workspaces))
	for i := range workspaces {
		runner, err := r.ensureExecutorCredential(ctx, &workspaces[i])
		if err != nil {
			return ctrl.Result{}, err
		}
		runners = append(runners, runner)
	}
	controller, err := r.ensureControllerClient(ctx, namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureFilesSecret(ctx, namespace, runners, []needy.StaticClient{controller}); err != nil {
		return ctrl.Result{}, err
	}
	if r.Config.DaemonStorageClass != "" {
		if err := r.ensureStateClaim(ctx, namespace); err != nil {
			return ctrl.Result{}, err
		}
	}
	replicas := int32(0)
	if len(workspaces) > 0 || live {
		replicas = 1
	}
	if err := r.ensureDaemon(ctx, namespace, replicas); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, namespace); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureNetworkPolicies(ctx, namespace); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// needyUsers returns the namespace's executor Workspaces and whether any
// Actor still needs the daemon.
func (r *NeedyReconciler) needyUsers(ctx context.Context, namespace string) ([]workspacesv1alpha1.Workspace, bool, error) {
	var workspaceList workspacesv1alpha1.WorkspaceList
	if err := r.List(ctx, &workspaceList, client.InNamespace(namespace)); err != nil {
		return nil, false, fmt.Errorf("list Workspaces: %w", err)
	}
	var workspaces []workspacesv1alpha1.Workspace
	for _, workspace := range workspaceList.Items {
		if workspace.DeletionTimestamp == nil && workspace.Spec.Needy != nil && workspace.Spec.Needy.Executor != nil {
			workspaces = append(workspaces, workspace)
		}
	}
	var actors workspacesv1alpha1.ActorList
	if err := r.List(ctx, &actors, client.InNamespace(namespace)); err != nil {
		return nil, false, fmt.Errorf("list Actors: %w", err)
	}
	for _, actor := range actors.Items {
		// A deleting Actor still needs the daemon to stop its run.
		if !actor.Status.Phase.IsTerminal() || actor.DeletionTimestamp != nil {
			return workspaces, true, nil
		}
	}

	return workspaces, false, nil
}

// needyExecutorSecretName holds a Workspace's executor runner credential.
func needyExecutorSecretName(workspace string) string {
	return workspace + "-needy-runner"
}

// ensureExecutorCredential keeps one credential per executor Workspace. The
// Workspace owns its Secret, and the daemon learns only its hash.
func (r *NeedyReconciler) ensureExecutorCredential(ctx context.Context, workspace *workspacesv1alpha1.Workspace) (needy.StaticRunner, error) {
	name := needy.WorkspaceRunnerName(workspace.Name)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: workspace.Namespace, Name: needyExecutorSecretName(workspace.Name)}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = make(map[string]string)
		}
		secret.Labels[needy.ComponentLabel] = needy.ComponentExecutor
		secret.Labels[needy.WorkspaceLabel] = workspace.Name
		if !needy.CredentialMatchesRunner(string(secret.Data[needy.CredentialKey]), name) {
			credential, err := needy.NewRunnerCredential(name)
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{needy.CredentialKey: []byte(credential)}
		}

		return controllerutil.SetOwnerReference(workspace, secret, r.Scheme)
	})
	if err != nil {
		return needy.StaticRunner{}, fmt.Errorf("ensure needy executor credential of Workspace %s: %w", workspace.Name, err)
	}
	labels := map[string]string{needy.WorkspaceLabel: workspace.Name}
	maps.Copy(labels, workspace.Spec.Needy.Executor.Labels)

	return needy.StaticRunner{
		Name:           name,
		CredentialHash: needy.CredentialHash(string(secret.Data[needy.CredentialKey])),
		Roles:          []string{needy.RoleExecutor},
		Labels:         labels,
	}, nil
}

// ensureControllerClient keeps the manager's static admin token in a
// Secret that only the manager reads, and returns its clients file entry.
func (r *NeedyReconciler) ensureControllerClient(ctx context.Context, namespace string) (needy.StaticClient, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needy.ClientSecretName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{needy.ComponentLabel: needy.ComponentClient}
		if !needy.TokenMatchesClient(string(secret.Data[needy.ClientTokenKey]), needy.ControllerClientName) {
			token, err := needy.NewClientToken(needy.ControllerClientName)
			if err != nil {
				return err
			}
			secret.Data = map[string][]byte{needy.ClientTokenKey: []byte(token)}
		}

		return nil
	}); err != nil {
		return needy.StaticClient{}, fmt.Errorf("ensure needy client Secret: %w", err)
	}

	return needy.StaticClient{
		Name:      needy.ControllerClientName,
		Access:    "admin",
		TokenHash: needy.CredentialHash(string(secret.Data[needy.ClientTokenKey])),
	}, nil
}

// ensureFilesSecret writes the daemon's runners and clients files. They hold
// only hashes, and the daemon reloads them when they change.
func (r *NeedyReconciler) ensureFilesSecret(ctx context.Context, namespace string, runners []needy.StaticRunner, clients []needy.StaticClient) error {
	runnersFile, err := needy.RunnersFile(runners)
	if err != nil {
		return err
	}
	clientsFile, err := needy.ClientsFile(clients)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needy.FilesSecretName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{needy.ComponentLabel: needy.ComponentDaemon}
		secret.Data = map[string][]byte{
			needy.RunnersFileKey: runnersFile,
			needy.ClientsFileKey: clientsFile,
		}

		return nil
	}); err != nil {
		return fmt.Errorf("ensure needy daemon files Secret: %w", err)
	}

	return nil
}

func (r *NeedyReconciler) ensureStateClaim(ctx context.Context, namespace string) error {
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needy.StateClaimName}}
	err := r.Get(ctx, client.ObjectKeyFromObject(claim), claim)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get needy daemon state claim: %w", err)
	}
	storageClass := r.Config.DaemonStorageClass
	claim.Labels = map[string]string{needy.ComponentLabel: needy.ComponentDaemon}
	claim.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		StorageClassName: &storageClass,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(needyStateClaimSize)},
		},
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create needy daemon state claim: %w", err)
	}

	return nil
}

func (r *NeedyReconciler) ensureDaemon(ctx context.Context, namespace string, replicas int32) error {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needy.DaemonName}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = map[string]string{needy.ComponentLabel: needy.ComponentDaemon}
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: needyDaemonSelector()}
		deployment.Spec.Template = needyDaemonTemplate(r.Config)

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure needy daemon Deployment: %w", err)
	}

	return nil
}

func needyDaemonSelector() map[string]string {
	return map[string]string{
		needy.ComponentLabel:         needy.ComponentDaemon,
		"app.kubernetes.io/name":     needyAppName,
		"app.kubernetes.io/instance": needy.DaemonName,
	}
}

// needyDaemonTemplate runs `needy daemon serve` with the flags that a shared
// cluster needs: no host files, the static runners file, and the gateway
// blocked from cluster ranges.
func needyDaemonTemplate(config needy.Config) corev1.PodTemplateSpec {
	serve := needy.DaemonServeCommand(
		needyFilesDirectory+"/"+needy.RunnersFileKey,
		needyFilesDirectory+"/"+needy.ClientsFileKey,
		config.GatewayBlockCIDRs,
	)
	state := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	if config.DaemonStorageClass != "" {
		state = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: needy.StateClaimName}}
	}
	container := corev1.Container{
		Name:    needyDaemonContainer,
		Image:   config.Image,
		Command: []string{"sh", "-c", needyPullBaseScript + "exec " + shellJoin(serve)},
		Env:     needyEnvironment(),
		Ports: []corev1.ContainerPort{
			{
				Name:          needyControlPort,
				ContainerPort: needy.ControlPort,
			},
			{
				Name:          needyGatewayPort,
				ContainerPort: needy.GatewayPort,
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: needy.DaemonCheckCommand()}},
			PeriodSeconds:  5,
			TimeoutSeconds: 5,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      needyStateVolume,
				MountPath: needyStateDirectory,
			},
			{
				Name:      needyDataVolume,
				MountPath: needyDataDirectory,
			},
			{
				Name:      needyRuntimeVolume,
				MountPath: needyRuntimeDirectory,
			},
			{
				Name:      needyFilesVolume,
				MountPath: needyFilesDirectory,
				ReadOnly:  true,
			},
		},
		SecurityContext: needyContainerSecurityContext(),
	}
	automount := false
	spec := corev1.PodSpec{
		AutomountServiceAccountToken:  &automount,
		TerminationGracePeriodSeconds: new(int64(15)),
		SecurityContext:               needyPodSecurityContext(config, false),
		Containers:                    []corev1.Container{container},
		Volumes: []corev1.Volume{
			{
				Name:         needyStateVolume,
				VolumeSource: state,
			},
			{
				Name:         needyDataVolume,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			{
				Name:         needyRuntimeVolume,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
			},
			{
				Name: needyFilesVolume,
				// A directory mount, never subPath: a subPath mount does not
				// receive updates, and needy reloads both files.
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  needy.FilesSecretName,
					DefaultMode: new(int32(0o440)),
				}},
			},
		},
	}
	if config.DaemonKVM {
		applyNeedyKVM(&spec, config)
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: needyDaemonSelector()},
		Spec:       spec,
	}
}

// needyPullBaseScript fetches the base VM image of the binary's own version.
// The daemon needs it to plan image layers even without KVM. The release
// download sometimes ends early, so it is retried. needy refuses a runtime
// directory that other users can open, and an emptyDir is world-writable, so
// the script first makes a private one inside it.
const needyPullBaseScript = `set -e
mkdir -p -m 0700 "$NEEDY_RUNTIME_DIR"
for attempt in 1 2 3 4; do
  needy image pull --driver linux-firecracker -q && break
  [ "$attempt" -lt 4 ] || exit 1
  sleep $((attempt * 10))
done
`

func needyEnvironment() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name:  "NEEDY_STATE_DIR",
			Value: needyStateDirectory + "/state",
		},
		{
			Name:  "NEEDY_CACHE_DIR",
			Value: needyStateDirectory + "/cache",
		},
		{
			Name:  "NEEDY_DATA_DIR",
			Value: needyDataDirectory,
		},
		{
			Name:  "NEEDY_RUNTIME_DIR",
			Value: needyRuntimeDirectory + "/private",
		},
		{
			Name:  "RUST_LOG",
			Value: "info",
		},
	}
}

func needyPodSecurityContext(config needy.Config, kvm bool) *corev1.PodSecurityContext {
	security := &corev1.PodSecurityContext{
		RunAsUser:    new(needyDaemonUser),
		RunAsGroup:   new(needyDaemonUser),
		RunAsNonRoot: new(true),
		FSGroup:      new(needyDaemonUser),
	}
	if kvm && config.KVMGroup != 0 {
		security.SupplementalGroups = []int64{config.KVMGroup}
	}

	return security
}

func needyContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{corev1.Capability("ALL")}},
	}
}

// applyNeedyKVM schedules a Pod on a KVM node and gives its first container
// the KVM device.
func applyNeedyKVM(spec *corev1.PodSpec, config needy.Config) {
	spec.NodeSelector = config.KVMNodeSelector
	spec.Tolerations = config.KVMTolerations
	if config.KVMGroup != 0 {
		spec.SecurityContext.SupplementalGroups = []int64{config.KVMGroup}
	}
	container := &spec.Containers[0]
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	container.Resources.Limits[config.KVMResource] = resource.MustParse("1")
}

func (r *NeedyReconciler) ensureService(ctx context.Context, namespace string) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: needy.DaemonName}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = map[string]string{needy.ComponentLabel: needy.ComponentDaemon}
		service.Spec.Selector = needyDaemonSelector()
		service.Spec.Ports = []corev1.ServicePort{
			{
				Name:       needyControlPort,
				Port:       needy.ControlPort,
				TargetPort: intstr.FromInt32(needy.ControlPort),
			},
			{
				Name:       needyGatewayPort,
				Port:       needy.GatewayPort,
				TargetPort: intstr.FromInt32(needy.GatewayPort),
			},
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("ensure needy daemon Service: %w", err)
	}

	return nil
}

// shellJoin quotes argv for sh -c.
func shellJoin(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, argument := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(argument, "'", `'\''`)+"'")
	}

	return strings.Join(quoted, " ")
}

func needyNamespaceRequest(_ context.Context, object client.Object) []reconcile.Request {
	return []reconcile.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: object.GetNamespace(),
				Name:      needy.DaemonName,
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NeedyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	needyObjects := builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
		_, managed := object.GetLabels()[needy.ComponentLabel]
		return managed
	}))

	return ctrl.NewControllerManagedBy(mgr).
		Named("workspaces-needy").
		Watches(&workspacesv1alpha1.Workspace{}, handler.EnqueueRequestsFromMapFunc(needyNamespaceRequest)).
		Watches(&workspacesv1alpha1.Actor{}, handler.EnqueueRequestsFromMapFunc(needyNamespaceRequest)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(needyNamespaceRequest), needyObjects).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(needyNamespaceRequest), builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return object.GetLabels()[needy.ComponentLabel] == needy.ComponentDaemon
		}))).
		Complete(r)
}
