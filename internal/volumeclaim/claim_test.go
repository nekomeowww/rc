package volumeclaim

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	workspaces "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const testName = "demo"

func TestNamesAreStableAndSeparated(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "repository-demo", Name(Repository, testName, 0))
	assert.Equal(t, "worktree-demo", Name(Worktree, testName, 0))
	assert.Equal(t, "workspace-demo-home", Name(WorkspaceHome, testName, 0))
	assert.Equal(t, "environment-demo-current-1", Name(EnvironmentCurrent, testName, 1))
	assert.Equal(t, "environment-demo-draft-2", Name(EnvironmentDraft, testName, 2))
	seen := map[string]bool{}
	for _, role := range []Role{Repository, Worktree, WorkspaceHome, EnvironmentCurrent, EnvironmentDraft} {
		for _, name := range []string{testName, "a.b", strings.Repeat("a", 253), strings.Repeat("a", 252) + "b", strings.Repeat("a", 217) + "." + strings.Repeat("b", 35)} {
			actual := Name(role, name, 42)
			assert.Equal(t, actual, Name(role, name, 42))
			assert.LessOrEqual(t, len(actual), 253)
			assert.Empty(t, validation.IsDNS1123Subdomain(actual))
			assert.False(t, seen[actual], "distinct roles and untruncated inputs need distinct names: %s", actual)
			seen[actual] = true
		}
	}
	assert.NotEqual(t, Name(EnvironmentDraft, strings.Repeat("a", 253), 1), Name(EnvironmentDraft, strings.Repeat("a", 253), 2))
	// A fixed vector detects accidental changes to the on-disk naming contract.
	assert.Equal(t, "repository-"+strings.Repeat("a", 225)+"-5eee79914246a9e1", Name(Repository, strings.Repeat("a", 253), 0))
}

func TestNamesSeparateLegacyDerivedCollisions(t *testing.T) {
	t.Parallel()
	for _, role := range []Role{Repository, Worktree, WorkspaceHome} {
		for _, environment := range []struct {
			role     Role
			revision int64
			suffix   string
		}{
			{role: EnvironmentCurrent, revision: 1, suffix: "-current-1"},
			{role: EnvironmentDraft, revision: 2, suffix: "-draft-2"},
		} {
			t.Run(string(role)+"/"+string(environment.role), func(t *testing.T) {
				derived := testName + environment.suffix
				assert.Equal(t, legacyName(role, derived, 0), legacyName(environment.role, testName, environment.revision), "legacy names collide regardless of creation order")
				assert.NotEqual(t, Name(role, derived, 0), Name(environment.role, testName, environment.revision), "typed names isolate both creation orders")
			})
		}
	}
}

func TestResolvePreservesOwnedClaimsAndRejectsForeignReferences(t *testing.T) {
	t.Parallel()
	for _, role := range []Role{Repository, Worktree, WorkspaceHome, EnvironmentCurrent, EnvironmentDraft} {
		t.Run(string(role), func(t *testing.T) {
			owner := &workspaces.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: "default", UID: types.UID("current")}}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, workspaces.AddToScheme(scheme))
			legacy := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: legacyName(role, owner.Name, 1), Namespace: owner.Namespace}}
			require.NoError(t, controllerutil.SetControllerReference(owner, legacy, scheme))
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
			name, claim, err := Resolve(t.Context(), kube, owner, role, 1, "")
			require.NoError(t, err)
			assert.Equal(t, legacy.Name, name, "recover legacy ownership if status update was interrupted")
			require.NotNil(t, claim)
			assert.Equal(t, legacy.Name, claim.Name)
			name, claim, err = Resolve(t.Context(), kube, owner, role, 1, legacy.Name)
			require.NoError(t, err)
			assert.Equal(t, legacy.Name, name)
			require.NotNil(t, claim)
			assert.Equal(t, legacy.Name, claim.Name)
			// A recorded arbitrary name catches recomputing CR-derived names,
			// even when an owned legacy claim is also available for recovery.
			recorded := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "recorded-storage", Namespace: owner.Namespace}}
			require.NoError(t, controllerutil.SetControllerReference(owner, recorded, scheme))
			require.NoError(t, kube.Create(t.Context(), recorded))
			name, claim, err = Resolve(t.Context(), kube, owner, role, 1, recorded.Name)
			require.NoError(t, err)
			assert.Equal(t, recorded.Name, name)
			require.NotNil(t, claim)
			assert.Equal(t, recorded.Name, claim.Name)
			owner.UID = "recreated"
			_, _, err = Resolve(t.Context(), kube, owner, role, 1, legacy.Name)
			require.True(t, IsConflict(err), "a recorded PVC from another UID must never be adopted")
			assert.ErrorContains(t, err, "current")
			name, claim, err = Resolve(t.Context(), kube, owner, role, 1, "")
			require.NoError(t, err)
			assert.Equal(t, Name(role, owner.Name, 1), name, "foreign legacy names do not block new typed storage")
			assert.Nil(t, claim, "an absent typed claim is reported as nil")
		})
	}
}

func TestPreflightRejectsOccupiedClaimsForEveryKind(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		owner    client.Object
		role     Role
		revision int64
	}{
		{owner: &repositories.Repository{}, role: Repository},
		{owner: &repositories.Worktree{}, role: Worktree},
		{owner: &workspaces.Workspace{}, role: WorkspaceHome},
		{owner: &workspaces.WorkspaceEnvironment{}, role: EnvironmentCurrent, revision: 7},
	} {
		t.Run(string(scenario.role), func(t *testing.T) {
			owner := scenario.owner
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			owner.SetName(testName)
			owner.SetNamespace("default")
			kube := fake.NewClientBuilder().WithScheme(scheme).Build()
			require.NoError(t, Preflight(t.Context(), kube, owner, scenario.role, scenario.revision))
			claimName := Name(scenario.role, owner.GetName(), scenario.revision)
			require.NoError(t, kube.Create(t.Context(), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: owner.GetNamespace()}}))
			err := Preflight(t.Context(), kube, owner, scenario.role, scenario.revision)
			require.True(t, IsConflict(err))
			assert.ErrorContains(t, err, claimName)
			assert.ErrorContains(t, err, "no controller owner")
		})
	}
}

func TestResolvePropagatesReadFailures(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	denied := apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), testName, fmt.Errorf("read denied"))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return denied
		},
	}).Build()
	owner := &workspaces.Workspace{ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: "default", UID: "uid"}}
	_, _, err := Resolve(t.Context(), kube, owner, WorkspaceHome, 0, "")
	require.ErrorIs(t, err, denied)
	require.ErrorIs(t, Preflight(t.Context(), kube, owner, WorkspaceHome, 0), denied)
}
