package workspaces

import (
	"testing"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const transcriptTestNamespace = "transcript-test"

func TestTranscriptResolverReadsEnvironmentDraftThenCurrent(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacesv1alpha1.AddToScheme(scheme))
	environment := &workspacesv1alpha1.WorkspaceEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "environment", Namespace: transcriptTestNamespace},
		Status:     workspacesv1alpha1.WorkspaceEnvironmentStatus{DraftVolumeClaimName: "draft", CurrentVolumeClaimName: "current"},
	}
	process := &workspacesv1alpha1.WorkspaceExec{
		ObjectMeta: metav1.ObjectMeta{Namespace: transcriptTestNamespace},
		Spec: workspacesv1alpha1.WorkspaceExecSpec{
			TargetRef: workspacesv1alpha1.WorkspaceExecTargetReference{Kind: workspacesv1alpha1.WorkspaceExecTargetWorkspaceEnvironment, Name: environment.Name},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment).Build()
	volume, err := ResolveTranscriptVolume(t.Context(), kube, process)
	require.NoError(t, err)
	require.Equal(t, "draft", volume.Claim)

	environment.Status.DraftVolumeClaimName = ""
	kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(environment).Build()
	volume, err = ResolveTranscriptVolume(t.Context(), kube, process)
	require.NoError(t, err)
	require.Equal(t, "current", volume.Claim, "fall back to the committed volume without a draft")
}
