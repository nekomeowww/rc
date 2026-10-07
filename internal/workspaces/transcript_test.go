package workspaces

import (
	"bytes"
	"testing"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const transcriptTestNamespace = "transcript-test"

func TestTranscriptLogsReportRetentionWithoutOpeningRuntime(t *testing.T) {
	t.Parallel()
	deleted := metav1.Now()
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Name: "expired"}, Status: workspacesv1alpha1.WorkspaceExecStatus{Conditions: []metav1.Condition{{Type: workspacesv1alpha1.WorkspaceExecConditionTranscriptCleanup, Status: metav1.ConditionTrue, LastTransitionTime: deleted}}}}
	err := (&ProcessClient{}).Logs(t.Context(), process, new(bytes.Buffer))
	require.ErrorContains(t, err, "was removed at")
}

func TestTranscriptResolverDoesNotFollowEnvironmentToNewDraft(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	environment := &workspacesv1alpha1.WorkspaceEnvironment{ObjectMeta: metav1.ObjectMeta{Name: "environment", Namespace: transcriptTestNamespace}, Status: workspacesv1alpha1.WorkspaceEnvironmentStatus{DraftVolumeClaimName: "new-draft", CurrentVolumeClaimName: "new-current"}}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: transcriptTestNamespace, UID: "original-uid"}}
	process := &workspacesv1alpha1.WorkspaceExec{ObjectMeta: metav1.ObjectMeta{Namespace: transcriptTestNamespace}, Spec: workspacesv1alpha1.WorkspaceExecSpec{TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment, Name: environment.Name}}, Status: workspacesv1alpha1.WorkspaceExecStatus{TranscriptVolumeClaimName: "original", TranscriptVolumeClaimUID: "original-uid"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment, claim).Build()
	volume, err := ResolveTranscriptVolume(t.Context(), kube, process)
	require.NoError(t, err)
	require.Equal(t, "original", volume.Claim)
	require.Equal(t, "original-uid", volume.ClaimUID, "resolution reports the PVC it already read")
	require.False(t, volume.ClaimDeleting)
	process.Status.TranscriptVolumeClaimUID = "deleted-uid"
	_, err = ResolveTranscriptVolume(t.Context(), kube, process)
	require.ErrorContains(t, err, "PVC was replaced")
}
