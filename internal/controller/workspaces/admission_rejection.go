package workspaces

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

const (
	// admissionRetryInitial and admissionRetryMax bound the wait between
	// creates the API server rejected. The wait doubles per rejection, so a
	// freed quota is picked up within a minute without hammering the server.
	admissionRetryInitial = 5 * time.Second
	admissionRetryMax     = time.Minute
	// admissionMessageLimit keeps the API server's explanation readable in a
	// Condition message; quota and Pod Security messages fit well within it.
	admissionMessageLimit = 1024

	admissionObjectRuntimePod = "runtime-pod"
	admissionObjectHome       = "home-volume-claim"
	admissionObjectHotMount   = "hot-mount"
)

// admissionRejected reports whether a create failed because the API server
// refused the object on policy grounds rather than because of a transient or
// server fault. Forbidden covers ResourceQuota, LimitRange, Pod Security
// admission, and denials from admission webhooks and ValidatingAdmissionPolicy;
// Invalid covers webhooks and policies that report a denial as a validation
// failure, as well as field validation the controller cannot repair by itself.
// A Namespace that is terminating also answers Forbidden; it is not a policy
// decision and the Workspace is about to be deleted with it.
func admissionRejected(err error) bool {
	if errors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		return false
	}

	return errors.IsForbidden(err) || errors.IsInvalid(err)
}

// admissionRejectionMessage names the rejected object and carries the API
// server's message, trimmed to admissionMessageLimit runes.
func admissionRejectionMessage(subject string, err error) string {
	message := []rune(fmt.Sprintf("%s was rejected by the API server: %s", subject, err.Error()))
	if len(message) > admissionMessageLimit {
		return string(message[:admissionMessageLimit-3]) + "..."
	}

	return string(message)
}

// admissionRetryKey scopes a backoff to one object of one Workspace
// generation, so a spec change retries at once instead of waiting.
func admissionRetryKey(workspace *workspacesv1alpha1.Workspace, object string) string {
	return string(workspace.UID) + "/" + strconv.FormatInt(workspace.Generation, 10) + "/" + object
}

// admissionRetries tracks, per key, how long to wait before repeating a create
// the API server rejected. It lives in controller memory: a restarted
// controller retries at once, then backs off again.
type admissionRetries struct {
	mu sync.Mutex
	// now is replaceable in tests; nil means time.Now.
	now     func() time.Time
	entries map[string]admissionRetryEntry
}

type admissionRetryEntry struct {
	delay   time.Duration
	retryAt time.Time
}

func (retries *admissionRetries) clock() time.Time {
	if retries.now != nil {
		return retries.now()
	}

	return time.Now()
}

// pending returns the remaining wait when the last create for key was
// rejected recently. Reconciles triggered by unrelated events then do not
// repeat a create the API server has just refused.
func (retries *admissionRetries) pending(key string) (time.Duration, bool) {
	retries.mu.Lock()
	defer retries.mu.Unlock()
	entry, found := retries.entries[key]
	if !found {
		return 0, false
	}
	remaining := entry.retryAt.Sub(retries.clock())
	if remaining <= 0 {
		return 0, false
	}

	return remaining, true
}

// record doubles the wait for key, from admissionRetryInitial up to
// admissionRetryMax, and returns it. A rejection long after the previous retry
// starts a new series.
func (retries *admissionRetries) record(key string) time.Duration {
	retries.mu.Lock()
	defer retries.mu.Unlock()
	now := retries.clock()
	if retries.entries == nil {
		retries.entries = map[string]admissionRetryEntry{}
	}
	// Drop entries of deleted Workspaces and superseded generations.
	for name, entry := range retries.entries {
		if now.Sub(entry.retryAt) > 2*admissionRetryMax {
			delete(retries.entries, name)
		}
	}
	delay := admissionRetryInitial
	if entry, found := retries.entries[key]; found {
		delay = min(2*entry.delay, admissionRetryMax)
	}
	retries.entries[key] = admissionRetryEntry{delay: delay, retryAt: now.Add(delay)}

	return delay
}

// clear forgets key after a create succeeds.
func (retries *admissionRetries) clear(key string) {
	retries.mu.Lock()
	defer retries.mu.Unlock()
	delete(retries.entries, key)
}
