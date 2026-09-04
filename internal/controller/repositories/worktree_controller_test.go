package repositories

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/worktreebootstrap"
	"github.com/nekomeowww/rc/internal/worktreeclaim"
)

const (
	generatedWorkspaceTestName = "generated-workspace"
	worktreeVolumeTestName     = "worktree"
	testMountName              = "source"
	testDeveloperName          = "developer"
)

var _ = Describe("Worktree Controller", func() {
	It("defers generated checkout to the consuming Workspace runtime", func() {
		ctx := context.Background()
		const (
			repositoryName = "deferred-parent"
			worktreeName   = "deferred-child"
		)
		repository := readyRepository(repositoryName)
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		repository.Status = readyRepositoryStatus(repositoryName)
		Expect(k8sClient.Status().Update(ctx, repository)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repository)).To(Succeed()) })

		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{
				Name: worktreeName, Namespace: testNamespace,
				Labels: map[string]string{generatedWorkspaceLabel: generatedWorkspaceTestName},
			},
			Spec: repositoriesv1alpha1.WorktreeSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repositoryName},
				Branch:        "rc/generated-workspace/repository",
			},
		}
		Expect(k8sClient.Create(ctx, worktree)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, worktree)).To(Succeed()) })
		key := types.NamespacedName{Name: worktreeName, Namespace: testNamespace}
		reconciler := &WorktreeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: "ghcr.io/example/rc/runner:test"}

		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		claim := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, key, claim)).To(Succeed())
		claim.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		persisted := new(repositoriesv1alpha1.Worktree)
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeConditionVolumeReady)).To(BeTrue())
		ready := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("WaitingForWorkspace"))
		job := new(batchv1.Job)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: worktreeBootstrapJobName(worktree), Namespace: testNamespace}, job))).To(BeTrue())

		containerName := worktreebootstrap.ContainerName(persisted.Namespace, persisted.Name, persisted.UID)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: generatedWorkspaceTestName, Namespace: testNamespace},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: containerName, Image: testRunnerImage, Command: []string{"true"}}},
				Containers:     []corev1.Container{{Name: "runtime", Image: testRunnerImage, Command: []string{"sleep", "3600"}}},
				Volumes: []corev1.Volume{{Name: worktreeVolumeTestName, VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: worktreeName},
				}}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, pod)).To(Succeed()) })
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: containerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)).To(BeTrue())
		Expect(persisted.Status.JobName).To(BeEmpty())
	})

	It("reuses the cloned Repository root for temporary Workspace Worktrees", func() {
		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "generated-worktree",
				Namespace: testNamespace,
				Labels:    map[string]string{generatedWorkspaceLabel: generatedWorkspaceTestName},
			},
			Spec: repositoriesv1alpha1.WorktreeSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: "repository-parent"},
				Branch:        "rc/generated-workspace/repository-parent",
			},
		}

		action := worktreebootstrap.Action(worktree.Spec.Branch, workerMountPath)
		Expect(worktreePath(worktree)).To(Equal(workerMountPath))
		Expect(action.Command).To(ContainElement(worktree.Spec.Branch))
		Expect(action.Command[2]).To(ContainSubstring("checkout -b"))
		Expect(action.Command[2]).NotTo(ContainSubstring("worktree add"))
	})

	It("clones the Repository PVC and creates a native Git worktree Job", func() {
		ctx := context.Background()
		const (
			repositoryName = "worktree-parent"
			worktreeName   = "worktree-child"
			runnerImage    = "ghcr.io/example/rc/runner:test"
		)
		repository := readyRepository(repositoryName)
		Expect(k8sClient.Create(ctx, repository)).To(Succeed())
		repository.Status = readyRepositoryStatus(repositoryName)
		Expect(k8sClient.Status().Update(ctx, repository)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, repository)).To(Succeed()) })

		worktree := &repositoriesv1alpha1.Worktree{
			ObjectMeta: metav1Object(worktreeName),
			Spec: repositoriesv1alpha1.WorktreeSpec{
				RepositoryRef: repositoriesv1alpha1.RepositoryReference{Name: repositoryName},
				Branch:        "feature/worktree",
			},
		}
		Expect(k8sClient.Create(ctx, worktree)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, worktree)).To(Succeed()) })

		reconciler := &WorktreeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: runnerImage}
		key := types.NamespacedName{Name: worktreeName, Namespace: testNamespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		claim := new(corev1.PersistentVolumeClaim)
		Expect(k8sClient.Get(ctx, key, claim)).To(Succeed())
		Expect(claim.Spec.DataSource.Kind).To(Equal("PersistentVolumeClaim"))
		Expect(claim.Spec.DataSource.Name).To(Equal(repositoryName))
		Expect(claim.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}))

		claim.Status.Phase = corev1.ClaimBound
		Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())

		workloadPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "worktree-workload", Namespace: testNamespace},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "workload",
					Image:   "busybox:latest",
					Command: []string{"sleep", "3600"},
				}},
				Volumes: []corev1.Volume{{
					Name: worktreeVolumeTestName,
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: worktreeName,
					}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, workloadPod)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, workloadPod)).To(Succeed()) })

		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		persistedPod := new(corev1.Pod)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workloadPod.Name, Namespace: testNamespace}, persistedPod)).To(Succeed())
		Expect(persistedPod.Labels[worktreeUIDLabel]).To(Equal(string(worktree.UID)))

		job := new(batchv1.Job)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: worktreeBootstrapJobName(worktree), Namespace: testNamespace}, job)).To(Succeed())
		container := job.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(runnerImage))
		Expect(container.Args).To(ContainElement(gitWorktreeSubcommand))
		Expect(container.Args).To(ContainElement("add"))
		Expect(container.Args).To(ContainElement("-b"))
		Expect(container.Args).To(ContainElement("feature/worktree"))
		Expect(container.Args).To(ContainElement("/mnt/rc/worktrees/worktree-child/worktree/worktree-child"))
		Expect(container.Args[1]).To(ContainSubstring("checkout.workers=8"))
		Expect(container.WorkingDir).To(Equal("/mnt/rc/worktrees/worktree-child"))
		Expect(container.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: workerVolumeName, MountPath: "/mnt/rc/worktrees/worktree-child"}))
		Expect(job.Spec.Template.Spec.SecurityContext.RunAsUser).To(HaveValue(Equal(int64(1000))))
		Expect(job.Spec.Template.Spec.SecurityContext.RunAsGroup).To(HaveValue(Equal(int64(1000))))
		Expect(job.Spec.Template.Spec.SecurityContext.FSGroup).To(HaveValue(Equal(int64(1000))))
		Expect(job.Spec.Template.Spec.SecurityContext.FSGroupChangePolicy).To(HaveValue(Equal(corev1.FSGroupChangeOnRootMismatch)))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(worktreeName))
		Expect(job.Spec.Template.Labels[worktreeUIDLabel]).To(Equal(string(worktree.UID)))
		Expect(metav1.IsControlledBy(job, worktree)).To(BeTrue())

		completedAt := metav1.Now()
		job.Status.StartTime = &completedAt
		job.Status.CompletionTime = &completedAt
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: completedAt},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completedAt},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		persisted := new(repositoriesv1alpha1.Worktree)
		Expect(k8sClient.Get(ctx, key, persisted)).To(Succeed())
		ready := meta.FindStatusCondition(persisted.Status.Conditions, repositoriesv1alpha1.WorktreeConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(persisted.Status.VolumeClaimName).To(Equal(worktreeName))
		Expect(persisted.Status.WorktreePath).To(Equal(worktreePath(worktree)))
	})

	It("creates a native Git worktree that remains usable at the runtime mount path", func() {
		// ROOT CAUSE:
		//
		// The bootstrap Job previously created the linked worktree below
		// /repository, but Workspace Pods mounted only that linked-worktree
		// subdirectory. Its .git file therefore referenced an absolute metadata
		// path that did not exist in the Workspace container. Creating the worktree
		// below the same stable volume root mounted by the runtime keeps both sides
		// of Git's native worktree link reachable.
		temporaryDirectory := GinkgoT().TempDir()
		stableRoot := filepath.Join(temporaryDirectory, "stable-root")
		stableTarget := filepath.Join(stableRoot, "worktree", "portable")

		runGitCommand(temporaryDirectory, "init", stableRoot)
		runGitCommand(stableRoot, "config", "user.name", "RC Test")
		runGitCommand(stableRoot, "config", "user.email", "rc@example.invalid")
		Expect(os.WriteFile(filepath.Join(stableRoot, "README.md"), []byte("portable worktree\n"), 0o600)).To(Succeed())
		runGitCommand(stableRoot, "add", "README.md")
		runGitCommand(stableRoot, "commit", "-m", "initial")

		testHome := filepath.Join(temporaryDirectory, "home")
		Expect(os.Mkdir(testHome, 0o700)).To(Succeed())
		command := exec.Command("sh", "-ceu", worktreeBootstrapScript, "worktree-bootstrap", "worktree", "add", "-b", "portable", stableTarget)
		command.Dir = stableRoot
		command.Env = append(os.Environ(), "HOME="+testHome)
		output, err := command.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(output))
		gitDirectory, err := os.ReadFile(filepath.Join(stableTarget, ".git"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(gitDirectory))).To(ContainSubstring(stableRoot))

		status := exec.Command("git", "status", "--short")
		status.Dir = stableTarget
		status.Env = append(os.Environ(), "HOME="+testHome)
		output, err = status.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(output))
	})

	It("keeps deletion protected while a Workspace references the Worktree", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(workspacesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		now := metav1.Now()
		worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{
			Name: "delete-referenced", Namespace: testNamespace, UID: testWorktreeUID, DeletionTimestamp: &now, Finalizers: []string{worktreeDeletionFinalizer},
		}}
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: testDeveloperName, Namespace: testNamespace},
			Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{
				Name: testMountName, Path: testMountName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name},
			}}},
		}
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, workspace).Build()
		reconciler := &WorktreeReconciler{Client: client, Scheme: scheme, RunnerImage: testRunnerImage}

		result, err := reconciler.reconcileDelete(context.Background(), worktree)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		persisted := new(repositoriesv1alpha1.Worktree)
		Expect(client.Get(context.Background(), types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}, persisted)).To(Succeed())
		Expect(persisted.Finalizers).To(ContainElement(worktreeDeletionFinalizer))
	})

	It("keeps deletion protected while an active writer holds the Lease", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(workspacesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		now := metav1.Now()
		worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{
			Name: "delete-in-use", Namespace: testNamespace, UID: testWorktreeUID, DeletionTimestamp: &now, Finalizers: []string{worktreeDeletionFinalizer},
		}}
		holder := testWorkspaceUID
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace, Labels: map[string]string{worktreeclaim.HolderLabel: testDeveloperName}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
		}
		client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree, lease).Build()
		reconciler := &WorktreeReconciler{Client: client, Scheme: scheme, RunnerImage: testRunnerImage}

		result, err := reconciler.reconcileDelete(context.Background(), worktree)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		persisted := new(repositoriesv1alpha1.Worktree)
		Expect(client.Get(context.Background(), types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}, persisted)).To(Succeed())
		Expect(persisted.Finalizers).To(ContainElement(worktreeDeletionFinalizer))
	})

	It("completes protected deletion after acquiring the exclusive Lease", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(workspacesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		now := metav1.Now()
		worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{
			Name: "delete-available", Namespace: testNamespace, UID: testWorktreeUID, DeletionTimestamp: &now, Finalizers: []string{worktreeDeletionFinalizer},
		}}
		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()
		reconciler := &WorktreeReconciler{Client: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage}

		result, err := reconciler.reconcileDelete(context.Background(), worktree)

		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))
		persisted := new(repositoriesv1alpha1.Worktree)
		err = kubeClient.Get(context.Background(), types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}, persisted)
		if err == nil {
			Expect(persisted.Finalizers).NotTo(ContainElement(worktreeDeletionFinalizer))
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	It("keeps deletion protected when a Workspace mount races the deletion claim", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(workspacesv1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		now := metav1.Now()
		worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{
			Name: "delete-mount-race", Namespace: testNamespace, UID: testWorktreeUID, DeletionTimestamp: &now, Finalizers: []string{worktreeDeletionFinalizer},
		}}
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "racing-developer", Namespace: testNamespace},
			Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{
				Name: testMountName, Path: testMountName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name},
			}}},
		}
		baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()
		raceClient := &workspaceMountRaceClient{Client: baseClient, workspace: workspace}
		reconciler := &WorktreeReconciler{Client: raceClient, Scheme: scheme, RunnerImage: testRunnerImage}

		result, err := reconciler.reconcileDelete(context.Background(), worktree)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		persisted := new(repositoriesv1alpha1.Worktree)
		Expect(baseClient.Get(context.Background(), types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}, persisted)).To(Succeed())
		Expect(persisted.Finalizers).To(ContainElement(worktreeDeletionFinalizer))
	})

	It("maps Workspace and foreign Lease changes back to the referenced Worktree", func() {
		scheme := runtime.NewScheme()
		Expect(repositoriesv1alpha1.AddToScheme(scheme)).To(Succeed())
		worktree := &repositoriesv1alpha1.Worktree{ObjectMeta: metav1.ObjectMeta{
			Name: "watched-worktree", Namespace: testNamespace, UID: "watched-worktree-uid",
		}}
		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worktree).Build()
		reconciler := &WorktreeReconciler{Client: kubeClient}
		workspace := &workspacesv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: testDeveloperName, Namespace: testNamespace},
			Spec: workspacesv1alpha1.WorkspaceSpec{Mounts: []workspacesv1alpha1.WorkspaceMount{{
				Name: testMountName, Path: testMountName, WorktreeRef: &workspacesv1alpha1.LocalReference{Name: worktree.Name},
			}}},
		}
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: worktreeclaim.LeaseName(worktree), Namespace: worktree.Namespace,
		}}
		request := reconcile.Request{NamespacedName: types.NamespacedName{Name: worktree.Name, Namespace: worktree.Namespace}}

		Expect(reconciler.worktreesForWorkspace(context.Background(), workspace)).To(ConsistOf(request))
		Expect(reconciler.worktreesForLease(context.Background(), lease)).To(ConsistOf(request))
	})
})

type workspaceMountRaceClient struct {
	client.Client
	workspace *workspacesv1alpha1.Workspace
	injected  bool
}

func (c *workspaceMountRaceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if _, ok := object.(*coordinationv1.Lease); !ok || c.injected {
		return nil
	}
	c.injected = true

	return c.Client.Create(ctx, c.workspace.DeepCopy())
}

func runGitCommand(directory string, arguments ...string) {
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), string(output))
}

func metav1Object(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace}
}
