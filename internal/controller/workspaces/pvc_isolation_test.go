//go:build integration

package workspaces

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	repositorycontrollers "github.com/nekomeowww/rc/internal/controller/repositories"
)

const (
	isolationRepository   = "Repository"
	isolationWorktree     = "Worktree"
	isolationWorkspace    = "Workspace"
	isolationEnvironment  = "Environment"
	isolationStorageClass = "test"
	isolationSourceClaim  = "source-volume"
)

// The real API assigns different UIDs across kinds. Fake-client-only tests
// previously missed the shared PVC namespace and the persistent conflict.
// envtest has no CSI provisioner: assert independent ownership and recorded
// identities, without requiring storage provisioning or Pod readiness.
var _ = Describe("PVC isolation", func() {
	var namespace *corev1.Namespace

	BeforeEach(func() {
		Expect(repositories.AddToScheme(k8sClient.Scheme())).To(Succeed())
		namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pvc-isolation-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
	})

	It("allocates four independently owned PVCs for four same-name CR kinds", func() {
		source, _ := isolationResource(ctx, isolationRepository, "clone-source", namespace.Name)
		repository := source.(*repositories.Repository)
		repository.Status = repositories.RepositoryStatus{ObservedGeneration: repository.Generation, VolumeClaimName: isolationSourceClaim, Conditions: []metav1.Condition{{
			Type: repositories.RepositoryConditionStorageReady, Status: metav1.ConditionTrue, Reason: "RepositoryReady", LastTransitionTime: metav1.Now(), ObservedGeneration: repository.Generation,
		}}}
		Expect(k8sClient.Status().Update(ctx, repository)).To(Succeed())

		claimNames := make([]string, 0, 4)
		for _, kind := range []string{isolationRepository, isolationWorktree, isolationWorkspace, isolationEnvironment} {
			owner, reconcileOwner := isolationResource(ctx, kind, "shared", namespace.Name)
			// Repeat reconciliation to catch persistent ownership conflicts.
			for range 2 {
				Expect(reconcileOwner()).To(Succeed())
			}
			claimName, reason := isolationStatus(ctx, owner)
			Expect(reason).To(BeElementOf("Provisioning", "Initializing"))
			Expect(claimName).NotTo(BeEmpty())
			Expect(claimNames).NotTo(ContainElement(claimName))
			claimNames = append(claimNames, claimName)
			claim := new(corev1.PersistentVolumeClaim)
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: claimName}, claim)).To(Succeed())
			Expect(metav1.IsControlledBy(claim, owner)).To(BeTrue())
		}
		claims := new(corev1.PersistentVolumeClaimList)
		Expect(k8sClient.List(ctx, claims, client.InNamespace(namespace.Name))).To(Succeed())
		Expect(claims.Items).To(HaveLen(4))
	})

	It("recovers a legacy Workspace home without adopting it as an Environment draft", func() {
		object, reconcileEnvironment := isolationResource(ctx, isolationEnvironment, "shared", namespace.Name)
		environment := object.(*workspaces.WorkspaceEnvironment)
		Expect(reconcileEnvironment()).To(Succeed())
		currentName, _ := isolationStatus(ctx, environment)
		current := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: currentName}, current)).To(Succeed())
		current.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())
		Expect(reconcileEnvironment()).To(Succeed())

		object, reconcileWorkspace := isolationResource(ctx, isolationWorkspace, "shared-draft-2", namespace.Name)
		workspace := object.(*workspaces.Workspace)
		workspace.Spec.EnvironmentRef = &workspaces.LocalReference{Name: environment.Name}
		workspace.Spec.Image, workspace.Spec.Storage = "", nil
		Expect(k8sClient.Update(ctx, workspace)).To(Succeed())
		// ROOT CAUSE:
		// A legacy Workspace home can have exactly the draft's derived name AND
		// clone spec. Spec-only checks allowed a foreign owner's PVC to be used.
		// Typed allocation and ownership checks must keep these volumes separate.
		legacy := environmentVolumeClaim(environment, workspace.Name, currentName)
		Expect(controllerutil.SetControllerReference(workspace, legacy, k8sClient.Scheme())).To(Succeed())
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		legacy.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, legacy)).To(Succeed())
		originalUID := legacy.UID

		// Empty status models a crash between legacy PVC creation and recording it.
		Expect(workspace.Status.HomeVolumeClaimName).To(BeEmpty())
		for range 2 {
			Expect(reconcileWorkspace()).To(Succeed())
		}
		homeName, reason := isolationStatus(ctx, workspace)
		Expect(reason).To(Equal(reasonStarting))
		Expect(homeName).To(Equal(legacy.Name))
		pods := new(corev1.PodList)
		Expect(k8sClient.List(ctx, pods, client.InNamespace(namespace.Name))).To(Succeed())
		Expect(pods.Items).To(HaveLen(1))
		Expect(pods.Items[0].Spec.Volumes).To(ContainElement(HaveField(persistentVolumeClaimKind, HaveField("ClaimName", Equal(legacy.Name)))))

		process := &workspaces.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name}, Spec: workspaces.WorkspaceExecSpec{TargetRef: workspaces.WorkspaceExecTargetReference{Name: environment.Name}}}
		execController := &WorkspaceExecReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for range 2 {
			_, reason, _, err := execController.resolveEnvironmentProcessTarget(ctx, process)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(reasonTargetNotReady))
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(environment), environment)).To(Succeed())
		Expect(environment.Status.DraftVolumeClaimName).To(Equal("environment-shared-draft-2"))
		draft := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: environment.Status.DraftVolumeClaimName}, draft)).To(Succeed())
		Expect(metav1.IsControlledBy(draft, environment)).To(BeTrue())
		Expect(draft.Spec).To(Equal(legacy.Spec), "matching clone specs must not permit foreign PVC adoption")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(legacy), legacy)).To(Succeed())
		Expect(legacy.UID).To(Equal(originalUID))
		Expect(metav1.IsControlledBy(legacy, workspace)).To(BeTrue())
		claims := new(corev1.PersistentVolumeClaimList)
		Expect(k8sClient.List(ctx, claims, client.InNamespace(namespace.Name))).To(Succeed())
		Expect(claims.Items).To(HaveLen(3), "retain current, new draft and the original legacy home")
	})
})

func isolationResource(ctx context.Context, kind, name, namespace string) (client.Object, func() error) {
	metadata := metav1.ObjectMeta{Name: name, Namespace: namespace}
	request := reconcile.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: namespace}}
	storage := workspaces.PersistentStorageSpec{StorageClassName: isolationStorageClass, Size: resource.MustParse("1Gi")}
	var object client.Object
	var controller reconcile.Reconciler
	switch kind {
	case isolationRepository:
		object = &repositories.Repository{ObjectMeta: metadata, Spec: repositories.RepositorySpec{
			Remote:  repositories.RepositoryRemoteSpec{URL: "https://example.test/repository.git"},
			Storage: repositories.RepositoryStorageSpec{StorageClassName: isolationStorageClass, Size: resource.MustParse("1Gi")},
		}}
		controller = &repositorycontrollers.RepositoryReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: testRunnerImage}
	case isolationWorktree:
		object = &repositories.Worktree{ObjectMeta: metadata, Spec: repositories.WorktreeSpec{RepositoryRef: repositories.RepositoryReference{Name: "clone-source"}, Branch: isolationStorageClass}}
		controller = &repositorycontrollers.WorktreeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: testRunnerImage}
	case isolationWorkspace:
		object = &workspaces.Workspace{ObjectMeta: metadata, Spec: workspaces.WorkspaceSpec{Image: testRunnerImage, Storage: &storage}}
		controller = &WorkspaceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: testRunnerImage}
	default:
		object = &workspaces.WorkspaceEnvironment{ObjectMeta: metadata, Spec: workspaces.WorkspaceEnvironmentSpec{Image: testRunnerImage, Storage: storage}}
		controller = &WorkspaceEnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}
	Expect(k8sClient.Create(ctx, object)).To(Succeed())
	return object, func() error { _, err := controller.Reconcile(ctx, request); return err }
}

func isolationStatus(ctx context.Context, object client.Object) (string, string) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(object), object)).To(Succeed())
	var claimName, conditionType string
	var conditions []metav1.Condition
	switch current := object.(type) {
	case *repositories.Repository:
		claimName, conditionType, conditions = current.Status.VolumeClaimName, repositories.RepositoryConditionStorageReady, current.Status.Conditions
	case *repositories.Worktree:
		claimName, conditionType, conditions = current.Status.VolumeClaimName, repositories.WorktreeConditionReady, current.Status.Conditions
	case *workspaces.Workspace:
		claimName, conditionType, conditions = current.Status.HomeVolumeClaimName, workspaces.WorkspaceConditionReady, current.Status.Conditions
	case *workspaces.WorkspaceEnvironment:
		claimName, conditionType, conditions = current.Status.CurrentVolumeClaimName, workspaces.WorkspaceEnvironmentConditionReady, current.Status.Conditions
	}
	condition := meta.FindStatusCondition(conditions, conditionType)
	Expect(condition).NotTo(BeNil())
	return claimName, condition.Reason
}
