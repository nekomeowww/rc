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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	testTenantLabel        = "example.com/tenant"
	testDoNotDisrupt       = "karpenter.sh/do-not-disrupt"
	testCostCenter         = "example.com/cost-center"
	testPriorityClass      = "workspace-high"
	testForeignAnnotation  = "example.com/added-by-another-controller"
	testPodMetadataSubject = "metadata"
	testTenantA            = "team-a"
	testForgedValue        = "forged"
)

func TestValidateWorkspacePodMetadata(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name     string
		metadata *workspacesv1alpha1.WorkspacePodMetadata
		reason   string
	}{
		{
			name: "omitted",
		},
		{
			name: "valid labels and annotations",
			metadata: &workspacesv1alpha1.WorkspacePodMetadata{
				Labels:      map[string]string{testTenantLabel: testTenantA},
				Annotations: map[string]string{testDoNotDisrupt: testTrueValue},
			},
		},
		{
			name: "label managed by rc",
			metadata: &workspacesv1alpha1.WorkspacePodMetadata{
				Labels: map[string]string{workspaceManagedByLabel: testForgedValue},
			},
			reason: reasonReservedPodMetadataKey,
		},
		{
			name: "annotation managed by rc",
			metadata: &workspacesv1alpha1.WorkspacePodMetadata{
				Annotations: map[string]string{workspaceTopologyAnnotation: testForgedValue},
			},
			reason: reasonReservedPodMetadataKey,
		},
		{
			name: "bare rc domain",
			metadata: &workspacesv1alpha1.WorkspacePodMetadata{
				Labels: map[string]string{"rc.ayaka.io/repository": testForgedValue},
			},
			reason: reasonReservedPodMetadataKey,
		},
		{
			name: "invalid label value",
			metadata: &workspacesv1alpha1.WorkspacePodMetadata{
				Labels: map[string]string{testTenantLabel: "team a"},
			},
			reason: reasonInvalidPodMetadata,
		},
		{
			name: "invalid annotation key",
			metadata: &workspacesv1alpha1.WorkspacePodMetadata{
				Annotations: map[string]string{"not a key": "value"},
			},
			reason: reasonInvalidPodMetadata,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			reason, message := validateWorkspacePodMetadata(scenario.metadata)
			assert.Equal(t, scenario.reason, reason, message)
		})
	}
}

func TestApplyWorkspacePodMetadataKeepsManagedKeysAndRemovesStaleKeys(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{
			workspaceManagedByLabel: testWorkspaceName,
		},
		Annotations: map[string]string{
			workspaceTopologyAnnotation: "topology",
			testForeignAnnotation:       "keep",
		},
	}}

	changed := applyWorkspacePodMetadata(pod, &workspacesv1alpha1.WorkspacePodMetadata{
		Labels: map[string]string{
			testTenantLabel:         testTenantA,
			workspaceManagedByLabel: testForgedValue,
		},
		Annotations: map[string]string{
			testDoNotDisrupt:            testTrueValue,
			workspaceTopologyAnnotation: testForgedValue,
		},
	})

	assertions.True(changed, "apply new keys")
	assertions.Equal(testWorkspaceName, pod.Labels[workspaceManagedByLabel], "rc label wins over a reserved user key")
	assertions.Equal("topology", pod.Annotations[workspaceTopologyAnnotation], "rc annotation wins over a reserved user key")
	assertions.Equal(testTenantA, pod.Labels[testTenantLabel])
	assertions.Equal(testTrueValue, pod.Annotations[testDoNotDisrupt])
	assertions.JSONEq(`{"labels":["example.com/tenant"],"annotations":["karpenter.sh/do-not-disrupt"]}`, pod.Annotations[workspacePodMetadataAnnotation], "record only the applied user keys")
	assertions.False(applyWorkspacePodMetadata(pod, &workspacesv1alpha1.WorkspacePodMetadata{
		Labels:      map[string]string{testTenantLabel: testTenantA},
		Annotations: map[string]string{testDoNotDisrupt: testTrueValue},
	}), "an unchanged spec does not change the Pod")

	changed = applyWorkspacePodMetadata(pod, &workspacesv1alpha1.WorkspacePodMetadata{
		Annotations: map[string]string{testCostCenter: "42"},
	})

	assertions.True(changed, "remove keys that are no longer requested")
	assertions.NotContains(pod.Labels, testTenantLabel)
	assertions.NotContains(pod.Annotations, testDoNotDisrupt)
	assertions.Equal("42", pod.Annotations[testCostCenter])
	assertions.Equal("keep", pod.Annotations[testForeignAnnotation], "keep keys that rc did not apply")
	assertions.Equal(testWorkspaceName, pod.Labels[workspaceManagedByLabel])

	assertions.True(applyWorkspacePodMetadata(pod, nil), "clearing podMetadata removes the applied keys")
	assertions.NotContains(pod.Annotations, testCostCenter)
	assertions.NotContains(pod.Annotations, workspacePodMetadataAnnotation, "drop the record when nothing is applied")
	assertions.Equal("keep", pod.Annotations[testForeignAnnotation])
}

func TestPriorityClassSupportKeepsExistingTopologyHash(t *testing.T) {
	t.Parallel()
	workspace := &workspacesv1alpha1.Workspace{}
	resolved := &resolvedWorkspace{runtime: testRCPlatform(t), image: testRunnerImage}

	baseHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	// Adding priorityClassName kept the hash of existing Pods, so that
	// upgrade did not replace them. Injecting rc-kube changed it once.
	assert.Equal(t, testLinuxTopologyHash, baseHash)

	workspace.Spec.PodMetadata = &workspacesv1alpha1.WorkspacePodMetadata{Labels: map[string]string{testTenantLabel: testTenantA}}
	metadataHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	assert.Equal(t, baseHash, metadataHash, "metadata is updated in place and is not part of the topology")

	workspace.Spec.PriorityClassName = testPriorityClass
	priorityHash, err := workspaceTopologyHash(workspace, resolved)
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, priorityHash, "priority is immutable on a Pod and changes the topology")
}

type podMetadataFixture struct {
	client     client.Client
	reconciler *WorkspaceReconciler
	key        types.NamespacedName
	workspace  *workspacesv1alpha1.Workspace
	podDeletes *atomic.Int32
}

func newPodMetadataFixture(t *testing.T, spec workspacesv1alpha1.WorkspaceSpec) podMetadataFixture {
	t.Helper()
	requirements := require.New(t)
	scheme := runtime.NewScheme()
	requirements.NoError(corev1.AddToScheme(scheme))
	requirements.NoError(coordinationv1.AddToScheme(scheme))
	requirements.NoError(rbacv1.AddToScheme(scheme))
	requirements.NoError(repositoriesv1alpha1.AddToScheme(scheme))
	requirements.NoError(workspacesv1alpha1.AddToScheme(scheme))
	spec.Image = testRunnerImage
	spec.Storage = &workspacesv1alpha1.PersistentStorageSpec{
		StorageClassName: testStorageClass,
		Size:             resource.MustParse("20Gi"),
	}
	workspace := &workspacesv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: testPodMetadataSubject, Namespace: testNamespace, UID: types.UID("metadata-uid")},
		Spec:       spec,
	}
	home := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspace.Name,
			Namespace: workspace.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				UID:        workspace.UID,
				Controller: boolPointer(true),
			}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	podDeletes := new(atomic.Int32)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace, home, &corev1.Pod{}, &workspacesv1alpha1.WorkspaceExec{}).
		WithObjects(workspace, home).
		WithIndex(&workspacesv1alpha1.WorkspaceExec{}, executionTargetIndex, executionTargetNames).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, kubeClient client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
				if _, ok := object.(*corev1.Pod); ok {
					podDeletes.Add(1)
				}
				return kubeClient.Delete(ctx, object, opts...)
			},
		}).
		Build()

	return podMetadataFixture{
		client:     kubeClient,
		reconciler: &WorkspaceReconciler{Client: kubeClient, APIReader: kubeClient, Scheme: scheme, RunnerImage: testRunnerImage},
		key:        client.ObjectKeyFromObject(workspace),
		workspace:  workspace,
		podDeletes: podDeletes,
	}
}

func (fixture podMetadataFixture) reconcile(t *testing.T) {
	t.Helper()
	_, err := fixture.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: fixture.key})
	require.NoError(t, err)
}

func (fixture podMetadataFixture) updateSpec(t *testing.T, mutate func(*workspacesv1alpha1.WorkspaceSpec)) {
	t.Helper()
	current := new(workspacesv1alpha1.Workspace)
	require.NoError(t, fixture.client.Get(context.Background(), fixture.key, current))
	mutate(&current.Spec)
	current.Generation++
	require.NoError(t, fixture.client.Update(context.Background(), current))
}

func (fixture podMetadataFixture) readyReason(t *testing.T) string {
	t.Helper()
	current := new(workspacesv1alpha1.Workspace)
	require.NoError(t, fixture.client.Get(context.Background(), fixture.key, current))
	ready := meta.FindStatusCondition(current.Status.Conditions, workspacesv1alpha1.WorkspaceConditionReady)
	require.NotNil(t, ready)
	return ready.Reason
}

func TestWorkspacePodMetadataChangePatchesRuntimeInPlace(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	fixture := newPodMetadataFixture(t, workspacesv1alpha1.WorkspaceSpec{
		PodMetadata: &workspacesv1alpha1.WorkspacePodMetadata{
			Labels:      map[string]string{testTenantLabel: testTenantA},
			Annotations: map[string]string{testDoNotDisrupt: testTrueValue},
		},
	})

	fixture.reconcile(t)
	pod := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, pod), "create the runtime Pod")
	assertions.Equal(testTenantA, pod.Labels[testTenantLabel], "apply user labels at creation")
	assertions.Equal(testTrueValue, pod.Annotations[testDoNotDisrupt], "apply user annotations at creation")
	assertions.Equal(fixture.workspace.Name, pod.Labels[workspaceManagedByLabel], "keep the rc label")
	topology := pod.Annotations[workspaceTopologyAnnotation]
	requirements.NotEmpty(topology)
	pod.Annotations[testForeignAnnotation] = "keep"
	requirements.NoError(fixture.client.Update(ctx, pod), "simulate another controller annotating the Pod")

	fixture.updateSpec(t, func(spec *workspacesv1alpha1.WorkspaceSpec) {
		spec.PodMetadata = &workspacesv1alpha1.WorkspacePodMetadata{
			Labels:      map[string]string{testTenantLabel: "team-b"},
			Annotations: map[string]string{testCostCenter: "42"},
		}
	})
	fixture.reconcile(t)

	updated := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, updated), "keep the runtime Pod")
	assertions.Zero(fixture.podDeletes.Load(), "a metadata change never deletes the runtime Pod")
	assertions.Equal(topology, updated.Annotations[workspaceTopologyAnnotation], "a metadata change keeps the topology")
	assertions.Equal("team-b", updated.Labels[testTenantLabel], "update a label in place")
	assertions.Equal("42", updated.Annotations[testCostCenter], "add an annotation in place")
	assertions.NotContains(updated.Annotations, testDoNotDisrupt, "remove an annotation dropped from the spec")
	assertions.Equal("keep", updated.Annotations[testForeignAnnotation], "keep annotations rc did not apply")
	assertions.Equal(fixture.workspace.Name, updated.Labels[workspaceManagedByLabel])
	assertions.Equal(reasonStarting, fixture.readyReason(t))
}

func TestWorkspacePriorityClassChangeRollsIdleRuntime(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx := context.Background()
	fixture := newPodMetadataFixture(t, workspacesv1alpha1.WorkspaceSpec{})

	fixture.reconcile(t)
	pod := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, pod), "create the runtime Pod")
	assertions.Empty(pod.Spec.PriorityClassName)
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Name: "priority-roll", Namespace: testNamespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{
				Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspace,
				Name: fixture.workspace.Name,
			},
			Command: []string{testTrueValue},
		},
		Status: workspacesv1alpha1.WorkspaceExecStatus{Phase: workspacesv1alpha1.WorkspaceExecPhaseRunning},
	}
	requirements.NoError(fixture.client.Create(ctx, process))
	requirements.NoError(fixture.client.Status().Update(ctx, process))

	fixture.updateSpec(t, func(spec *workspacesv1alpha1.WorkspaceSpec) {
		spec.PriorityClassName = testPriorityClass
		spec.PodMetadata = &workspacesv1alpha1.WorkspacePodMetadata{
			Labels: map[string]string{testTenantLabel: testTenantA},
		}
	})
	fixture.reconcile(t)

	blocked := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, blocked), "keep a runtime Pod with active executions")
	assertions.Zero(fixture.podDeletes.Load(), "never restart a runtime Pod with active executions")
	assertions.Equal("TopologyChangeBlocked", fixture.readyReason(t), "report the waiting priority change")
	assertions.Empty(blocked.Spec.PriorityClassName)
	assertions.Equal(testTenantA, blocked.Labels[testTenantLabel], "metadata still updates while the roll waits")

	requirements.NoError(fixture.client.Delete(ctx, process), "finish the active execution")
	fixture.reconcile(t)
	assertions.True(apierrors.IsNotFound(fixture.client.Get(ctx, fixture.key, new(corev1.Pod))), "replace the idle runtime Pod")
	assertions.Equal(int32(1), fixture.podDeletes.Load())
	assertions.Equal("Replacing", fixture.readyReason(t))

	fixture.reconcile(t)
	replacement := new(corev1.Pod)
	requirements.NoError(fixture.client.Get(ctx, fixture.key, replacement), "create the replacement runtime Pod")
	assertions.Equal(testPriorityClass, replacement.Spec.PriorityClassName, "the replacement uses the new PriorityClass")
	assertions.Equal(testTenantA, replacement.Labels[testTenantLabel])
}

func TestWorkspaceRejectsReservedPodMetadataBeforeCreatingRuntime(t *testing.T) {
	t.Parallel()
	fixture := newPodMetadataFixture(t, workspacesv1alpha1.WorkspaceSpec{
		PodMetadata: &workspacesv1alpha1.WorkspacePodMetadata{
			Annotations: map[string]string{workspaceRuntimePolicyAnnotation: testForgedValue},
		},
	})

	fixture.reconcile(t)

	assert.True(t, apierrors.IsNotFound(fixture.client.Get(context.Background(), fixture.key, new(corev1.Pod))), "create no runtime Pod")
	assert.Equal(t, reasonReservedPodMetadataKey, fixture.readyReason(t))
}
