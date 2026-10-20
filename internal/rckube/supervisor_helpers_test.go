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

package rckube

import (
	"testing"
	"time"
)

// newTestSupervisor stops every native child before the test's directories
// are removed. A child writes its record when it exits, so a test that returns
// while a child is still exiting would otherwise race TempDir cleanup.
// Cleanups run in reverse order, and this one is registered after the
// directories it uses.
func newTestSupervisor(t *testing.T, stateDirectory string, stopGrace time.Duration, options ...SupervisorOption) *Supervisor {
	t.Helper()
	supervisor := NewSupervisor(stateDirectory, stopGrace, options...)
	t.Cleanup(func() {
		supervisor.mu.RLock()
		ids := make([]string, 0, len(supervisor.processes))
		for id, process := range supervisor.processes {
			// Tests may insert a fake process without a native child.
			if process.native != nil {
				ids = append(ids, id)
			}
		}
		supervisor.mu.RUnlock()
		for _, id := range ids {
			_, _ = supervisor.Stop(id)
		}
	})

	return supervisor
}
