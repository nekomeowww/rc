package executionretention

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

func TestPolicyDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	require.Equal(t, processruntime.HistoryPolicy{
		TTLAfterFinished: DefaultTTL,
		MaxEntries:       DefaultMaxEntries,
		TranscriptTTL:    DefaultTranscriptTTL,
	}, Policy(nil), "retention is on without a policy")
	require.Equal(t, processruntime.HistoryPolicy{
		TTLAfterFinished: time.Hour,
		MaxEntries:       10,
		TranscriptTTL:    DefaultTranscriptTTL,
	}, Policy(&workspacesv1alpha1.ExecutionRetentionPolicy{
		TTLAfterFinished: &metav1.Duration{Duration: time.Hour},
		MaxEntries:       10,
	}), "override only the fields that are set")
}
