package executionretention

import (
	"testing"
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEffectiveAppliesBuildDefaults(t *testing.T) {
	ttl, maxEntries := Effective(nil)
	require.Zero(t, ttl)
	require.Zero(t, maxEntries)
	ttl, maxEntries = Effective(&workspacesv1alpha1.ExecutionRetentionPolicy{})
	require.Equal(t, DefaultTTL, ttl)
	require.Equal(t, int32(DefaultMaxEntries), maxEntries)
	ttl, maxEntries = Effective(&workspacesv1alpha1.ExecutionRetentionPolicy{TTLAfterFinished: &metav1.Duration{Duration: time.Hour}, MaxEntries: 3})
	require.Equal(t, time.Hour, ttl)
	require.Equal(t, int32(3), maxEntries)
}
