// Package executionretention resolves how long a supervisor keeps finished
// process history. The supervisor applies the policy to its own records.
package executionretention

import (
	"time"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	processruntime "github.com/nekomeowww/rc/internal/execution"
)

const (
	// DefaultTTL applies when TTLAfterFinished is unset or not positive.
	DefaultTTL = 90 * 24 * time.Hour
	// DefaultMaxEntries applies when MaxEntries is not positive.
	DefaultMaxEntries = 15000
	// DefaultTranscriptTTL applies when TranscriptTTL is unset or not positive.
	DefaultTranscriptTTL = 14 * 24 * time.Hour
)

// Policy resolves a target's executionRetention. Retention is on by default: a
// nil policy uses every default.
func Policy(policy *workspacesv1alpha1.ExecutionRetentionPolicy) processruntime.HistoryPolicy {
	resolved := processruntime.HistoryPolicy{
		TTLAfterFinished: DefaultTTL,
		MaxEntries:       DefaultMaxEntries,
		TranscriptTTL:    DefaultTranscriptTTL,
	}
	if policy == nil {
		return resolved
	}
	if policy.TTLAfterFinished != nil && policy.TTLAfterFinished.Duration > 0 {
		resolved.TTLAfterFinished = policy.TTLAfterFinished.Duration
	}
	if policy.MaxEntries > 0 {
		resolved.MaxEntries = int(policy.MaxEntries)
	}
	if policy.TranscriptTTL != nil && policy.TranscriptTTL.Duration > 0 {
		resolved.TranscriptTTL = policy.TranscriptTTL.Duration
	}

	return resolved
}
