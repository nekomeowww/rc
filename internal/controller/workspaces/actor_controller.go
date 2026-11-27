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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacesv1alpha1 "github.com/nekomeowww/rc/apis/kubernetes/workspaces/v1alpha1"
	needyv1 "github.com/nekomeowww/rc/apis/sdk/go/needy/v1"
	"github.com/nekomeowww/rc/pkg/needy"
)

const (
	actorFinalizer = "workspaces.rc.ayaka.io/actor"
	// actorCredentialRevokedAnnotation records that the credentials of the
	// actor runner and the external executor runners were revoked after the
	// actor runner's Pod ended.
	actorCredentialRevokedAnnotation = "workspaces.rc.ayaka.io/needy-credential-revoked"
	// actorCredentialMintedAnnotation records when rc minted the credential
	// in a runner Secret, in RFC 3339. An external runner must connect within
	// actorStartGrace of it.
	actorCredentialMintedAnnotation = "workspaces.rc.ayaka.io/needy-credential-minted-at"
	annotationTrue                  = "true"
	actorRunnerContainer            = "runner"
	// actorCredentialDirectory holds the runner credential. It is not under
	// /var/run, which is /run in many images, where needy keeps its sockets.
	actorCredentialDirectory = "/etc/needy/credential"
	// actorProxyDirectory holds the proxy's USERNAME:PASSWORD.
	actorProxyDirectory       = "/etc/needy/proxy"
	actorProxyCredentialsFile = "credentials"
	actorProxyVolume          = "proxy"
	// actorStartGrace covers the runner Pod's start, the base image download,
	// and image preparation before the run can start.
	actorStartGrace = 15 * time.Minute
	// actorCreateTimeout bounds CreateRun, which waits for the runner and
	// prepares the actor's image.
	actorCreateTimeout = 20 * time.Minute
	actorCallTimeout   = 30 * time.Second
	actorPoll          = 10 * time.Second
	actorStopGrace     = 10 * time.Second
	// actorPodLinger is how long a runner Pod may outlive its ended run before
	// it is deleted. An ephemeral runner exits on its own when the actor ends.
	actorPodLinger = time.Minute
	// actorDefaultMaxRunDuration applies when the API default is absent.
	actorDefaultMaxRunDuration = 2 * time.Hour
)

var errNeedyDaemonNotReady = errors.New("the namespace's needy daemon is not ready")

// ActorReconciler runs one needy run per Actor: it mints the credentials of
// the actor runner and of any external executor runners, starts the
// ephemeral runner Pod on a KVM node, creates the run on the namespace's
// daemon, and follows it until it ends. Status comes from
// the daemon's run record, never from the Pod's exit code: an ephemeral
// runner exits 0 whatever the actor's outcome.
type ActorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config NeedyConfig
	// Dialer defaults to needy.GRPCDialer.
	Dialer needy.Dialer
	// DaemonAddress overrides the daemon's in-cluster address, for a manager
	// that reaches the daemon another way.
	DaemonAddress func(namespace string) string
	// Now defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=actors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=actors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=actors/finalizers,verbs=update
// +kubebuilder:rbac:groups=workspaces.rc.ayaka.io,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods;secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves an Actor through Pending, Starting, Running, and one of the
// terminal phases.
func (r *ActorReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var actor workspacesv1alpha1.Actor
	if err := r.Get(ctx, request.NamespacedName, &actor); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if actor.DeletionTimestamp != nil {
		return r.finalize(ctx, &actor)
	}
	// Finalizers are patched: an update sends the whole spec, and the API
	// server compares it with the stored one for immutability, including
	// fields that only the typed object spells out.
	patch := client.MergeFrom(actor.DeepCopy())
	if controllerutil.AddFinalizer(&actor, actorFinalizer) {
		if err := r.Patch(ctx, &actor, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	if actor.Status.Phase.IsTerminal() {
		return r.cleanUp(ctx, &actor)
	}
	if reason := r.Config.ActorUnavailableReason(); reason != "" {
		return r.wait(ctx, &actor, workspacesv1alpha1.ActorReasonNeedyNotConfigured, reason, time.Minute)
	}
	daemon, err := r.dial(ctx, actor.Namespace)
	if errors.Is(err, errNeedyDaemonNotReady) {
		return r.wait(ctx, &actor, workspacesv1alpha1.ActorReasonWaitingForDaemon, "Waiting for the namespace's needy daemon", needyDaemonPoll)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		_ = daemon.Close()
	}()
	if actor.Status.RunID == "" {
		return r.start(ctx, &actor, daemon)
	}

	return r.follow(ctx, &actor, daemon)
}

// start mints the runner credential, starts the runner Pod, and creates the
// run. Every step can repeat: the credential Secret and the Pod are looked up
// first, and a run that was created before a crash is found by its name.
func (r *ActorReconciler) start(ctx context.Context, actor *workspacesv1alpha1.Actor, daemon needy.Daemon) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	executors, reason, message, err := r.executorRunners(ctx, actor)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		return r.wait(ctx, actor, reason, message, actorPoll)
	}
	text, profileName, err := r.profileText(ctx, actor)
	if err != nil {
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonInvalidProfile, err.Error())
	}
	env, reason, message, err := r.actorEnv(ctx, actor)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		return r.wait(ctx, actor, reason, message, actorPoll)
	}
	runner := needyActorRunnerName(string(actor.UID))
	profile, err := needy.ComposeProfile(text, runner, executors, env)
	if err != nil {
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonInvalidProfile, err.Error())
	}
	if err := validateActorPod(actor.Spec.Pod); err != nil {
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonInvalidPod, err.Error())
	}
	minted, err := r.ensureRunnerCredential(ctx, actor, daemon, actorRunnerCredential(actor))
	if err != nil {
		return ctrl.Result{}, err
	}
	if minted {
		log.Info("Minted needy actor runner credential", "runner", runner)
	}
	// Known from here on, so that a failure below still revokes it, and the
	// external executors' credentials with it.
	actor.Status.RunnerName = runner
	// An external runner cannot connect before it has a credential, so rc
	// mints them now and waits for the runners below.
	external := externalExecutors(actor)
	for _, executor := range external {
		minted, err := r.ensureRunnerCredential(ctx, actor, daemon, externalRunnerCredential(actor, executor))
		if err != nil {
			return ctrl.Result{}, err
		}
		if minted {
			log.Info("Minted needy external executor runner credential", "executor", executor.Name, "runner", executor.RunnerName)
		}
	}
	actor.Status.Executors = external
	proxy := actor.Spec.Proxy
	if err := r.ensureProxyPolicy(ctx, actor, proxy); err != nil {
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonInvalidProfile, err.Error())
	}
	pod, err := r.ensureRunnerPod(ctx, actor, runner, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonRunnerFailed, "The actor runner Pod ended before the run started")
	}
	if actor.Status.Phase != workspacesv1alpha1.ActorPhaseStarting {
		actor.Status.Phase = workspacesv1alpha1.ActorPhaseStarting
		actor.Status.PodName = pod.Name
		actor.Status.ObservedGeneration = actor.Generation
		setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonWaitingForRunner, "Waiting for the actor runner to connect")
		// Publish Starting before CreateRun, so a retry after a crash looks
		// for the run instead of creating a second one.
		if err := r.Status().Update(ctx, actor); err != nil {
			return ctrl.Result{}, err
		}
	}
	onlineContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
	online, err := daemon.IsActorRunnerConnected(onlineContext, runner)
	cancel()
	if err != nil {
		return ctrl.Result{}, err
	}
	if !online {
		return ctrl.Result{RequeueAfter: needyDaemonPoll}, nil
	}
	// needy's placement check refuses a runner that has not connected, so
	// the external runners must be online before it.
	if result, waiting, err := r.awaitExternalRunners(ctx, actor, daemon, external); waiting || err != nil {
		return result, err
	}
	checkContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
	err = daemon.CheckPlacement(checkContext, profile)
	cancel()
	if errors.Is(err, needy.ErrUnauthenticated) {
		return ctrl.Result{RequeueAfter: needyDaemonPoll}, nil
	}
	if err != nil {
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonInvalidProfile, err.Error())
	}
	setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonCreatingRun, "Creating the needy run")
	runName := actorRunName(actor)
	lookupContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
	run, err := daemon.GetRun(lookupContext, runName)
	cancel()
	if errors.Is(err, needy.ErrNotFound) {
		createContext, cancel := context.WithTimeout(ctx, actorCreateTimeout)
		run, err = daemon.CreateRun(createContext, needy.RunSpec{
			Name:          runName,
			ProfileName:   profileName,
			ProfileYAML:   profile,
			Args:          actor.Spec.Args,
			Terminal:      actor.Spec.Terminal,
			WaitForRunner: actorStartGrace,
		})
		cancel()
	}
	if errors.Is(err, needy.ErrUnauthenticated) {
		return ctrl.Result{RequeueAfter: needyDaemonPoll}, nil
	}
	if err != nil {
		// needy keeps a failed create as a failed run; the next attempt finds
		// it by name and publishes it.
		log.Error(err, "Could not create needy run", "run", runName)
		return ctrl.Result{RequeueAfter: actorPoll}, nil
	}
	log.Info("Created needy run", "run", run.ID)
	now := metav1.NewTime(r.now())
	actor.Status.RunID = run.ID
	actor.Status.StartedAt = &now
	r.publishRun(actor, run)
	if err := r.Status().Update(ctx, actor); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: actorPoll}, nil
}

// follow publishes the run and stops it at its idle or maximum deadline.
func (r *ActorReconciler) follow(ctx context.Context, actor *workspacesv1alpha1.Actor, daemon needy.Daemon) (ctrl.Result, error) {
	callContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
	run, err := daemon.GetRun(callContext, actor.Status.RunID)
	cancel()
	switch {
	case errors.Is(err, needy.ErrNotFound):
		// The daemon lost its state: the run cannot be followed or resumed.
		return r.fail(ctx, actor, workspacesv1alpha1.ActorReasonRunLost, "The needy daemon has no record of the run")
	case errors.Is(err, needy.ErrUnauthenticated):
		return ctrl.Result{RequeueAfter: needyDaemonPoll}, nil
	case err != nil:
		return ctrl.Result{}, err
	}
	if !run.IsTerminal() && actor.Status.StopReason == "" {
		if reason := r.stopReason(actor, run); reason != "" {
			stopContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
			err := daemon.StopRun(stopContext, run.ID, actorStopGrace)
			cancel()
			if err != nil && !errors.Is(err, needy.ErrNotFound) {
				return ctrl.Result{}, err
			}
			logf.FromContext(ctx).Info("Stopped needy run", "run", run.ID, "reason", reason)
			actor.Status.StopReason = reason
		}
	}
	r.publishRun(actor, run)
	if err := r.Status().Update(ctx, actor); err != nil {
		return ctrl.Result{}, err
	}
	if actor.Status.Phase.IsTerminal() {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: actorPoll}, nil
}

func (r *ActorReconciler) stopReason(actor *workspacesv1alpha1.Actor, run needy.Run) string {
	now := r.now()
	if actor.Status.StartedAt != nil && now.Sub(actor.Status.StartedAt.Time) >= actorMaxRunDuration(actor) {
		return workspacesv1alpha1.ActorStopReasonMaxRunDuration
	}
	idle := actor.Spec.IdleTimeout
	if idle == nil || idle.Duration <= 0 {
		return ""
	}
	last := run.LastActivity
	if last.IsZero() && actor.Status.StartedAt != nil {
		last = actor.Status.StartedAt.Time
	}
	if !last.IsZero() && now.Sub(last) >= idle.Duration {
		return workspacesv1alpha1.ActorStopReasonIdle
	}

	return ""
}

// publishRun copies the run record into the status.
func (r *ActorReconciler) publishRun(actor *workspacesv1alpha1.Actor, run needy.Run) {
	actor.Status.ObservedGeneration = actor.Generation
	actor.Status.RunState = strings.TrimPrefix(run.State.String(), "RUN_STATE_")
	actor.Status.ActorState = strings.TrimPrefix(run.ActorState.String(), "ACTOR_STATE_")
	actor.Status.ExitCode = run.ExitCode
	actor.Status.Message = run.Failure
	if !run.LastActivity.IsZero() {
		last := metav1.NewTime(run.LastActivity)
		actor.Status.LastActivityTime = &last
	}
	if !run.IsTerminal() {
		actor.Status.Phase = workspacesv1alpha1.ActorPhaseRunning
		setActorReady(actor, metav1.ConditionTrue, workspacesv1alpha1.ActorReasonRunning, "The needy run is live")
		return
	}
	switch {
	case run.State == needyv1.RunState_RUN_STATE_FAILED:
		actor.Status.Phase = workspacesv1alpha1.ActorPhaseFailed
		setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonRunFailed, run.Failure)
	case run.State == needyv1.RunState_RUN_STATE_STOPPED || actor.Status.StopReason != "":
		actor.Status.Phase = workspacesv1alpha1.ActorPhaseStopped
		setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonCompleted, "The needy run was stopped")
	case run.ExitCode != nil && *run.ExitCode == 0:
		actor.Status.Phase = workspacesv1alpha1.ActorPhaseSucceeded
		setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonCompleted, "The driving program exited")
	default:
		actor.Status.Phase = workspacesv1alpha1.ActorPhaseFailed
		setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonRunFailed, "The driving program exited with an error")
	}
	if actor.Status.CompletedAt == nil {
		completed := metav1.NewTime(r.now())
		actor.Status.CompletedAt = &completed
	}
}

// cleanUp ends what a terminal Actor still holds: the runner Pod, then the
// runner credential, then the Actor itself after deleteAfterCompleted. The
// credential is revoked only after the Pod has ended, because an ephemeral
// runner whose credential is refused exits with an error.
func (r *ActorReconciler) cleanUp(ctx context.Context, actor *workspacesv1alpha1.Actor) (ctrl.Result, error) {
	ended, result, err := r.endRunnerPod(ctx, actor)
	if err != nil || !ended {
		return result, err
	}
	if actor.Status.RunnerName != "" && actor.Annotations[actorCredentialRevokedAnnotation] == "" {
		if err := r.revoke(ctx, actor); err != nil {
			if errors.Is(err, errNeedyDaemonNotReady) {
				return ctrl.Result{RequeueAfter: needyDaemonPoll}, nil
			}
			return ctrl.Result{}, err
		}
		patch := client.MergeFrom(actor.DeepCopy())
		if actor.Annotations == nil {
			actor.Annotations = make(map[string]string)
		}
		actor.Annotations[actorCredentialRevokedAnnotation] = annotationTrue
		if err := r.Patch(ctx, actor, patch); err != nil {
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("Revoked needy runner credentials", "runner", actor.Status.RunnerName, "external", len(externalExecutors(actor)))
	}
	after := actor.Spec.DeleteAfterCompleted
	if after == nil || actor.Status.CompletedAt == nil {
		return ctrl.Result{}, nil
	}
	remaining := actor.Status.CompletedAt.Add(after.Duration).Sub(r.now())
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	logf.FromContext(ctx).Info("Deleting completed Actor")
	if err := r.Delete(ctx, actor); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// endRunnerPod reports whether the runner Pod has ended. A Pod that keeps
// running after its run ended is deleted.
func (r *ActorReconciler) endRunnerPod(ctx context.Context, actor *workspacesv1alpha1.Actor) (bool, ctrl.Result, error) {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: actorPodName(actor)}, &pod)
	if apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, nil
	}
	if err != nil {
		return false, ctrl.Result{}, err
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return true, ctrl.Result{}, nil
	}
	if actor.Status.CompletedAt != nil {
		if wait := actor.Status.CompletedAt.Add(actorPodLinger).Sub(r.now()); wait > 0 {
			return false, ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	if pod.DeletionTimestamp == nil {
		if err := r.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, err
		}
	}

	return false, ctrl.Result{RequeueAfter: actorPoll}, nil
}

// finalize stops a live run before the Actor goes away. A namespace whose
// daemon is gone has nothing left to stop.
func (r *ActorReconciler) finalize(ctx context.Context, actor *workspacesv1alpha1.Actor) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(actor, actorFinalizer) {
		return ctrl.Result{}, nil
	}
	if actor.Status.RunID != "" && !actor.Status.Phase.IsTerminal() {
		daemon, err := r.dial(ctx, actor.Namespace)
		switch {
		case errors.Is(err, errNeedyDaemonNotReady):
			if !r.daemonGone(ctx, actor.Namespace) {
				return ctrl.Result{RequeueAfter: needyDaemonPoll}, nil
			}
		case err != nil:
			return ctrl.Result{}, err
		default:
			stopContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
			err = daemon.StopRun(stopContext, actor.Status.RunID, actorStopGrace)
			cancel()
			_ = daemon.Close()
			if err != nil && !errors.Is(err, needy.ErrNotFound) {
				return ctrl.Result{}, err
			}
			logf.FromContext(ctx).Info("Stopped needy run of deleted Actor", "run", actor.Status.RunID)
		}
	}
	if actor.Status.RunnerName != "" && actor.Annotations[actorCredentialRevokedAnnotation] == "" {
		if err := r.revoke(ctx, actor); err != nil && !errors.Is(err, errNeedyDaemonNotReady) {
			return ctrl.Result{}, err
		}
	}
	patch := client.MergeFrom(actor.DeepCopy())
	controllerutil.RemoveFinalizer(actor, actorFinalizer)

	return ctrl.Result{}, r.Patch(ctx, actor, patch)
}

func (r *ActorReconciler) daemonGone(ctx context.Context, namespace string) bool {
	var service corev1.Service
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: needyDaemonName}, &service)

	return apierrors.IsNotFound(err)
}

// revoke revokes the actor runner's credential and the credentials of the
// external executors' runners. The external runner names come from the spec,
// not the status, so a credential minted before a lost status update is
// revoked too; a name that was never minted is not found.
func (r *ActorReconciler) revoke(ctx context.Context, actor *workspacesv1alpha1.Actor) error {
	daemon, err := r.dial(ctx, actor.Namespace)
	if err != nil {
		return err
	}
	defer func() {
		_ = daemon.Close()
	}()
	callContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
	defer cancel()
	runners := []string{actor.Status.RunnerName}
	for _, executor := range externalExecutors(actor) {
		runners = append(runners, executor.RunnerName)
	}
	for _, runner := range runners {
		if err := daemon.RevokeRunnerCredential(callContext, runner); err != nil && !errors.Is(err, needy.ErrNotFound) {
			return err
		}
	}

	return nil
}

// executorRunners maps the Actor's executor bindings to runner names. Every
// bound Workspace must serve as a needy executor. An external binding's
// runner name is derived from the Actor; its runner connects later.
func (r *ActorReconciler) executorRunners(ctx context.Context, actor *workspacesv1alpha1.Actor) (map[string]string, string, string, error) {
	runners := make(map[string]string, len(actor.Spec.Executors))
	for _, binding := range actor.Spec.Executors {
		if binding.External != nil {
			runners[binding.Name] = needyExternalRunnerName(string(actor.UID), binding.Name)
			continue
		}
		if binding.WorkspaceRef == nil {
			// The CRD requires one of the two; this guards a nil reference.
			return nil, workspacesv1alpha1.ActorReasonExecutorNotReady, fmt.Sprintf("Executor %s sets neither workspaceRef nor external", binding.Name), nil
		}
		var workspace workspacesv1alpha1.Workspace
		err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: binding.WorkspaceRef.Name}, &workspace)
		if apierrors.IsNotFound(err) {
			return nil, workspacesv1alpha1.ActorReasonExecutorNotReady, fmt.Sprintf("Workspace %s of executor %s does not exist", binding.WorkspaceRef.Name, binding.Name), nil
		}
		if err != nil {
			return nil, "", "", err
		}
		if workspace.Spec.Needy == nil || workspace.Spec.Needy.Executor == nil {
			return nil, workspacesv1alpha1.ActorReasonExecutorNotReady, fmt.Sprintf("Workspace %s of executor %s does not set spec.needy.executor", workspace.Name, binding.Name), nil
		}
		runners[binding.Name] = needyWorkspaceRunnerName(workspace.Name)
	}

	return runners, "", "", nil
}

// actorEnv reads spec.env. A missing Secret, ConfigMap, or key that is not
// optional is a reason to wait, like an executor that is not ready.
//
// NOTICE: the values go into the profile that the daemon keeps with the run
// and gives the guest, so a Secret's value is stored in the namespace's
// daemon state. needy has no reference to a value it could read later; the
// daemon is in the same namespace and tenant as the Secret.
func (r *ActorReconciler) actorEnv(ctx context.Context, actor *workspacesv1alpha1.Actor) (map[string]string, string, string, error) {
	if len(actor.Spec.Env) == 0 {
		return nil, "", "", nil
	}
	env := make(map[string]string, len(actor.Spec.Env))
	for _, variable := range actor.Spec.Env {
		source := variable.ValueFrom
		if source == nil {
			env[variable.Name] = variable.Value
			continue
		}
		var data map[string]string
		var kind, name, key string
		var optional *bool
		switch {
		case source.SecretKeyRef != nil:
			kind, name, key, optional = "Secret", source.SecretKeyRef.Name, source.SecretKeyRef.Key, source.SecretKeyRef.Optional
			var secret corev1.Secret
			err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: name}, &secret)
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, "", "", fmt.Errorf("get env Secret %s: %w", name, err)
			}
			if err == nil {
				data = make(map[string]string, len(secret.Data))
				for dataKey, value := range secret.Data {
					data[dataKey] = string(value)
				}
			}
		case source.ConfigMapKeyRef != nil:
			kind, name, key, optional = "ConfigMap", source.ConfigMapKeyRef.Name, source.ConfigMapKeyRef.Key, source.ConfigMapKeyRef.Optional
			var configMap corev1.ConfigMap
			err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: name}, &configMap)
			if err != nil && !apierrors.IsNotFound(err) {
				return nil, "", "", fmt.Errorf("get env ConfigMap %s: %w", name, err)
			}
			if err == nil {
				data = configMap.Data
			}
		default:
			// The CRD admits only the two sources; an older stored object may not.
			return nil, workspacesv1alpha1.ActorReasonEnvNotReady, fmt.Sprintf("env %s must come from a value, a Secret key, or a ConfigMap key", variable.Name), nil
		}
		value, found := data[key]
		if found {
			env[variable.Name] = value
			continue
		}
		if optional != nil && *optional {
			continue
		}
		if data == nil {
			return nil, workspacesv1alpha1.ActorReasonEnvNotReady, fmt.Sprintf("%s %s of env %s does not exist", kind, name, variable.Name), nil
		}
		return nil, workspacesv1alpha1.ActorReasonEnvNotReady, fmt.Sprintf("%s %s has no key %s for env %s", kind, name, key, variable.Name), nil
	}

	return env, "", "", nil
}

// profileText returns the profile YAML and the name needy records for it.
func (r *ActorReconciler) profileText(ctx context.Context, actor *workspacesv1alpha1.Actor) (string, string, error) {
	profile := actor.Spec.Profile
	switch {
	case profile.Builtin != "":
		return needy.BuiltinProfile(profile.Builtin), "builtin:" + profile.Builtin, nil
	case profile.ConfigMapKeyRef != nil:
		var configMap corev1.ConfigMap
		reference := profile.ConfigMapKeyRef
		if err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: reference.Name}, &configMap); err != nil {
			return "", "", fmt.Errorf("read profile ConfigMap %s: %w", reference.Name, err)
		}
		text, found := configMap.Data[reference.Key]
		if !found {
			return "", "", fmt.Errorf("profile ConfigMap %s has no key %s", reference.Name, reference.Key)
		}
		return text, "configmap:" + reference.Name + "/" + reference.Key, nil
	default:
		return profile.Inline, "actor:" + actor.Name, nil
	}
}

// runnerCredential is a runner credential that an Actor owns: the runner it
// is minted for, the one role that runner may declare, and the Secret that
// keeps it.
type runnerCredential struct {
	runner string
	role   needy.Role
	secret string
	labels map[string]string
}

func actorRunnerCredential(actor *workspacesv1alpha1.Actor) runnerCredential {
	return runnerCredential{
		runner: needyActorRunnerName(string(actor.UID)),
		role:   needy.RoleActor,
		secret: actorCredentialSecretName(actor),
		labels: map[string]string{workspacesv1alpha1.NeedyComponentLabel: needyComponentActor},
	}
}

func externalRunnerCredential(actor *workspacesv1alpha1.Actor, executor workspacesv1alpha1.ActorExecutorStatus) runnerCredential {
	return runnerCredential{
		runner: executor.RunnerName,
		role:   needy.RoleExecutor,
		secret: executor.CredentialSecretName,
		labels: map[string]string{
			workspacesv1alpha1.NeedyComponentLabel: needyComponentExecutor,
			workspacesv1alpha1.NeedyActorLabel:     actor.Name,
		},
	}
}

// externalExecutors derives the runner and credential Secret of every
// external executor binding. Both are fixed by the Actor's UID and the
// executor's name.
func externalExecutors(actor *workspacesv1alpha1.Actor) []workspacesv1alpha1.ActorExecutorStatus {
	var executors []workspacesv1alpha1.ActorExecutorStatus
	for _, binding := range actor.Spec.Executors {
		if binding.External == nil {
			continue
		}
		runner := needyExternalRunnerName(string(actor.UID), binding.Name)
		executors = append(executors, workspacesv1alpha1.ActorExecutorStatus{
			Name:       binding.Name,
			RunnerName: runner,
			// The runner name carries the UID, so the Secret is fresh for every
			// Actor, and the executor name is already spelled as a DNS label.
			CredentialSecretName: actor.Name + "-" + runner,
		})
	}

	return executors
}

// ensureRunnerCredential mints a runner's credential once and keeps it in a
// Secret that the Actor owns. Every credential lives as long as the actor
// runner's: maxRunDuration plus the start grace. It reports whether it minted
// one.
func (r *ActorReconciler) ensureRunnerCredential(ctx context.Context, actor *workspacesv1alpha1.Actor, daemon needy.Daemon, wanted runnerCredential) (bool, error) {
	key := types.NamespacedName{Namespace: actor.Namespace, Name: wanted.secret}
	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	if err == nil && needy.IsMintedCredential(string(existing.Data[needyCredentialKey]), wanted.runner) {
		return false, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	callContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
	defer cancel()
	// A credential minted before a crash, but never stored, still holds the
	// name; revoking it first lets the name be minted again.
	if err := daemon.RevokeRunnerCredential(callContext, wanted.runner); err != nil && !errors.Is(err, needy.ErrNotFound) {
		return false, err
	}
	credential, err := daemon.MintRunnerCredential(callContext, wanted.runner, wanted.role, actorMaxRunDuration(actor)+actorStartGrace)
	if err != nil {
		return false, err
	}
	minted := r.now().UTC().Format(time.RFC3339)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = wanted.labels
		secret.Annotations = map[string]string{actorCredentialMintedAnnotation: minted}
		secret.Data = map[string][]byte{needyCredentialKey: []byte(credential)}

		return controllerutil.SetControllerReference(actor, secret, r.Scheme)
	}); err != nil {
		return false, err
	}

	return true, nil
}

// awaitExternalRunners waits for the runners of the external executors to
// connect. A runner that has not connected within actorStartGrace of its
// credential's minting fails the Actor, as an actor runner Pod that ends
// before the run does. It reports whether the caller must wait.
func (r *ActorReconciler) awaitExternalRunners(ctx context.Context, actor *workspacesv1alpha1.Actor, daemon needy.Daemon, executors []workspacesv1alpha1.ActorExecutorStatus) (ctrl.Result, bool, error) {
	for _, executor := range executors {
		callContext, cancel := context.WithTimeout(ctx, actorCallTimeout)
		online, err := daemon.IsExecutorRunnerConnected(callContext, executor.RunnerName)
		cancel()
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if online {
			continue
		}
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: executor.CredentialSecretName}, &secret); err != nil {
			return ctrl.Result{}, true, err
		}
		minted, err := time.Parse(time.RFC3339, secret.Annotations[actorCredentialMintedAnnotation])
		if err != nil {
			minted = secret.CreationTimestamp.Time
		}
		if !r.now().Before(minted.Add(actorStartGrace)) {
			result, err := r.fail(ctx, actor, workspacesv1alpha1.ActorReasonExecutorOffline,
				fmt.Sprintf("The runner %s of external executor %s did not connect within %s", executor.RunnerName, executor.Name, actorStartGrace))

			return result, true, err
		}
		message := fmt.Sprintf("Waiting for the runner %s of external executor %s to connect", executor.RunnerName, executor.Name)
		if setActorReady(actor, metav1.ConditionFalse, workspacesv1alpha1.ActorReasonWaitingForRunner, message) {
			if err := r.Status().Update(ctx, actor); err != nil {
				return ctrl.Result{}, true, err
			}
		}

		return ctrl.Result{RequeueAfter: needyDaemonPoll}, true, nil
	}

	return ctrl.Result{}, false, nil
}

func (r *ActorReconciler) ensureRunnerPod(ctx context.Context, actor *workspacesv1alpha1.Actor, runner string, proxy *workspacesv1alpha1.ActorProxy) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: actor.Namespace, Name: actorPodName(actor)}, &pod)
	if err == nil {
		return &pod, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	desired := actorRunnerPod(actor, r.Config, proxy)
	if err := controllerutil.SetControllerReference(actor, desired, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, desired); err != nil {
		return nil, err
	}
	logf.FromContext(ctx).Info("Created needy actor runner Pod", "pod", desired.Name, "runner", runner)

	return desired, nil
}

// actorRunnerPod runs the ephemeral actor runner. The runner never listens:
// it dials the daemon's control port and gateway, and the VM has no network
// interface. The credential stays in the runner and never enters the VM.
func actorRunnerPod(actor *workspacesv1alpha1.Actor, config NeedyConfig, proxy *workspacesv1alpha1.ActorProxy) *corev1.Pod {
	options := needy.ActorRunnerOptions{
		Address:        "tcp://" + needyDaemonAddress(actor.Namespace),
		Gateway:        "tcp://" + needyGatewayAddress(actor.Namespace),
		CredentialFile: actorCredentialDirectory + "/" + needyCredentialKey,
		WorkDirectory:  needyDataDirectory + "/vm",
	}
	if proxy != nil {
		options.Upstream = proxy.URL
		if proxy.CredentialsSecretRef != nil {
			options.UpstreamCredentialsFile = actorProxyDirectory + "/" + actorProxyCredentialsFile
		}
	}
	serve := needy.ActorRunnerCommand(options)
	resources := *actor.Spec.Resources.DeepCopy()
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("1500m"),
			corev1.ResourceMemory:           resource.MustParse("4Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
		}
	}
	automount := false
	deadline := int64((actorMaxRunDuration(actor) + actorStartGrace).Seconds())
	spec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		AutomountServiceAccountToken:  &automount,
		ActiveDeadlineSeconds:         &deadline,
		TerminationGracePeriodSeconds: new(int64(30)),
		SecurityContext:               needyPodSecurityContext(config, true),
		Containers: []corev1.Container{
			{
				Name:      actorRunnerContainer,
				Image:     config.Image,
				Command:   []string{"sh", "-c", needyPullBaseScript + "mkdir -p " + needyDataDirectory + "/vm\nexec " + shellescape.QuoteCommand(serve)},
				Env:       needyEnvironment(),
				Resources: resources,
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      needyDataVolume,
						MountPath: needyDataDirectory,
					},
					{
						Name:      needyStateVolume,
						MountPath: needyStateDirectory,
					},
					{
						Name:      needyRuntimeVolume,
						MountPath: needyRuntimeDirectory,
					},
					{
						Name:      "credential",
						MountPath: actorCredentialDirectory,
						ReadOnly:  true,
					},
				},
				SecurityContext: needyContainerSecurityContext(),
			},
		},
		Volumes: []corev1.Volume{
			{
				Name:         needyDataVolume,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			{
				Name:         needyStateVolume,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			{
				Name:         needyRuntimeVolume,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
			},
			{
				Name: "credential",
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  actorCredentialSecretName(actor),
					DefaultMode: new(int32(0o440)),
				}},
			},
		},
	}
	if reference := proxyCredentialsReference(proxy); reference != nil {
		// needy refuses a credentials file that other users can read; 0440
		// with the Pod's fsGroup is the narrowest a Secret volume gives.
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: actorProxyVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: reference.Name,
				Items: []corev1.KeyToPath{
					{
						Key:  reference.Key,
						Path: actorProxyCredentialsFile,
					},
				},
				DefaultMode: new(int32(0o440)),
			}},
		})
		spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      actorProxyVolume,
			MountPath: actorProxyDirectory,
			ReadOnly:  true,
		})
	}
	applyNeedyKVM(&spec, config)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: actor.Namespace,
			Name:      actorPodName(actor),
			Labels: map[string]string{
				workspacesv1alpha1.NeedyComponentLabel: needyComponentActor,
				workspacesv1alpha1.NeedyActorLabel:     actor.Name,
				appNameLabel:                           needyAppName,
			},
			// A node consolidation would end the run.
			Annotations: map[string]string{"karpenter.sh/do-not-disrupt": annotationTrue},
		},
		Spec: spec,
	}
	applyActorPod(pod, actor.Spec.Pod)

	return pod
}

// applyActorPod adds spec.pod to the runner Pod. What rc already set wins:
// the manager's KVM node selector, rc's labels, and rc's annotations.
func applyActorPod(pod *corev1.Pod, shape *workspacesv1alpha1.ActorPod) {
	if shape == nil {
		return
	}
	if shape.Metadata != nil {
		addMissing(pod.Labels, shape.Metadata.Labels)
		addMissing(pod.Annotations, shape.Metadata.Annotations)
	}
	if len(shape.NodeSelector) > 0 {
		// The manager's selector is shared configuration; build a new map.
		selector := maps.Clone(shape.NodeSelector)
		maps.Copy(selector, pod.Spec.NodeSelector)
		pod.Spec.NodeSelector = selector
	}
	pod.Spec.Tolerations = append(slices.Clone(pod.Spec.Tolerations), shape.Tolerations...)
	pod.Spec.Affinity = shape.Affinity.DeepCopy()
	pod.Spec.PriorityClassName = shape.PriorityClassName
	pod.Spec.ImagePullSecrets = slices.Clone(shape.ImagePullSecrets)
}

// addMissing copies the keys of extra that target lacks and that rc does not
// reserve.
func addMissing(target, extra map[string]string) {
	for key, value := range extra {
		if _, set := target[key]; set || workspacesv1alpha1.IsReservedPodMetadataKey(key) {
			continue
		}
		target[key] = value
	}
}

// validateActorPod checks the label and annotation syntax that the CRD does
// not express, so a bad key fails the Actor instead of every Pod create.
func validateActorPod(shape *workspacesv1alpha1.ActorPod) error {
	if shape == nil || shape.Metadata == nil {
		return nil
	}
	path := field.NewPath("spec", "pod", "metadata")
	errs := metav1validation.ValidateLabels(shape.Metadata.Labels, path.Child("labels"))
	errs = append(errs, apivalidation.ValidateAnnotations(shape.Metadata.Annotations, path.Child("annotations"))...)

	return errs.ToAggregate()
}

func proxyCredentialsReference(proxy *workspacesv1alpha1.ActorProxy) *corev1.SecretKeySelector {
	if proxy == nil {
		return nil
	}

	return proxy.CredentialsSecretRef
}

// ensureProxyPolicy admits the actor runner Pod to its proxy before the
// Pod starts. The Actor owns the policy. Without the manager's
// NetworkPolicies nothing restricts the Pod, and a policy for the proxy
// alone would restrict it to the proxy.
func (r *ActorReconciler) ensureProxyPolicy(ctx context.Context, actor *workspacesv1alpha1.Actor, proxy *workspacesv1alpha1.ActorProxy) error {
	if proxy == nil || !r.Config.NetworkPolicies {
		return nil
	}
	desired, err := actorProxyPolicy(actor, proxy)
	if err != nil || desired == nil {
		return err
	}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: actor.Namespace, Name: desired.Name}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = desired.Labels
		policy.Spec = desired.Spec

		return controllerutil.SetControllerReference(actor, policy, r.Scheme)
	})

	return err
}

func (r *ActorReconciler) dial(ctx context.Context, namespace string) (needy.Daemon, error) {
	var deployment appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: needyDaemonName}, &deployment)
	if apierrors.IsNotFound(err) || (err == nil && deployment.Status.AvailableReplicas == 0) {
		return nil, errNeedyDaemonNotReady
	}
	if err != nil {
		return nil, err
	}
	var secret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: needyClientSecretName}, &secret)
	if apierrors.IsNotFound(err) || (err == nil && len(secret.Data[needyClientTokenKey]) == 0) {
		return nil, errNeedyDaemonNotReady
	}
	if err != nil {
		return nil, err
	}
	dialer := r.Dialer
	if dialer == nil {
		dialer = needy.GRPCDialer{}
	}

	address := needyDaemonAddress(namespace)
	if r.DaemonAddress != nil {
		address = r.DaemonAddress(namespace)
	}

	return dialer.Dial(ctx, address, string(secret.Data[needyClientTokenKey]))
}

// wait publishes a Pending reason without failing the Actor.
func (r *ActorReconciler) wait(ctx context.Context, actor *workspacesv1alpha1.Actor, reason, message string, after time.Duration) (ctrl.Result, error) {
	changed := actor.Status.Phase == ""
	if changed {
		actor.Status.Phase = workspacesv1alpha1.ActorPhasePending
	}
	actor.Status.ObservedGeneration = actor.Generation
	if setActorReady(actor, metav1.ConditionFalse, reason, message) || changed {
		if err := r.Status().Update(ctx, actor); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: after}, nil
}

// fail ends the Actor without a run, or with a run that cannot be followed.
func (r *ActorReconciler) fail(ctx context.Context, actor *workspacesv1alpha1.Actor, reason, message string) (ctrl.Result, error) {
	logf.FromContext(ctx).Info("Actor failed", "reason", reason, "message", message)
	actor.Status.Phase = workspacesv1alpha1.ActorPhaseFailed
	actor.Status.Message = message
	actor.Status.ObservedGeneration = actor.Generation
	now := metav1.NewTime(r.now())
	actor.Status.CompletedAt = &now
	setActorReady(actor, metav1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, actor); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func setActorReady(actor *workspacesv1alpha1.Actor, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(&actor.Status.Conditions, metav1.Condition{
		Type:               workspacesv1alpha1.ActorConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: actor.Generation,
	})
}

func actorMaxRunDuration(actor *workspacesv1alpha1.Actor) time.Duration {
	if actor.Spec.MaxRunDuration == nil || actor.Spec.MaxRunDuration.Duration <= 0 {
		return actorDefaultMaxRunDuration
	}

	return actor.Spec.MaxRunDuration.Duration
}

func actorPodName(actor *workspacesv1alpha1.Actor) string {
	return actor.Name + "-runner"
}

func actorCredentialSecretName(actor *workspacesv1alpha1.Actor) string {
	return actor.Name + "-needy-runner"
}

// actorRunName names the run in needy. needy run names are not unique; the
// UID suffix keeps a lookup after a crash unambiguous.
func actorRunName(actor *workspacesv1alpha1.Actor) string {
	return actor.Name + "-" + strings.ReplaceAll(string(actor.UID), "-", "")[:8]
}

func (r *ActorReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

// waitingActors requeues the namespace's Actors that have not started, when a
// Workspace or the daemon's Deployment changes.
func (r *ActorReconciler) waitingActors(ctx context.Context, object client.Object) []reconcile.Request {
	var actors workspacesv1alpha1.ActorList
	if err := r.List(ctx, &actors, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, actor := range actors.Items {
		if actor.Status.RunID == "" {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&actor)})
		}
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ActorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	daemonDeployment := builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetName() == needyDaemonName
	}))

	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacesv1alpha1.Actor{}).
		Owns(&corev1.Pod{}).
		Watches(&workspacesv1alpha1.Workspace{}, handler.EnqueueRequestsFromMapFunc(r.waitingActors)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.waitingActors), daemonDeployment).
		// CreateRun waits for the runner and the image; one slow start must
		// not hold every other Actor.
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Named("workspaces-actor").
		Complete(r)
}
