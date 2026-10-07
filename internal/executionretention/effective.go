package executionretention

import (
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// Effective returns the TTL and count limit that Build applies for policy.
// A nil policy has no limits and returns zero values.
func Effective(policy *workspacesv1alpha1.ExecutionRetentionPolicy) (ttl time.Duration, maxEntries int32) {
	if policy == nil {
		return 0, 0
	}
	ttl, maxEntries = DefaultTTL, policy.MaxEntries
	if policy.TTLAfterFinished != nil && policy.TTLAfterFinished.Duration > 0 {
		ttl = policy.TTLAfterFinished.Duration
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return ttl, maxEntries
}
