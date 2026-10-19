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

package repositoryaccess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

func TestFailure(t *testing.T) {
	t.Parallel()
	repository := func(status metav1.ConditionStatus, reason string, observed int64) *repositories.Repository {
		return &repositories.Repository{
			ObjectMeta: metav1.ObjectMeta{Name: "parent", Generation: 2},
			Status: repositories.RepositoryStatus{
				ObservedGeneration: observed,
				Conditions: []metav1.Condition{{
					Type:               repositories.RepositoryConditionStorageReady,
					Status:             status,
					Reason:             reason,
					Message:            "fetch failed",
					ObservedGeneration: observed,
				}},
			},
		}
	}

	message, failed := Failure(repository(metav1.ConditionFalse, "SyncFailed", 2))
	assert.True(t, failed, "a failed sync needs a new RepositorySync")
	assert.Equal(t, "Repository parent is not usable (SyncFailed): fetch failed", message)

	for _, test := range []struct {
		name     string
		status   metav1.ConditionStatus
		reason   string
		observed int64
	}{
		{"Ready", metav1.ConditionTrue, "RepositoryReady", 2},
		{"SyncRunning", metav1.ConditionFalse, "SyncRunning", 2},
		{"Initializing", metav1.ConditionFalse, "Initializing", 2},
		{"StaleGeneration", metav1.ConditionFalse, "CredentialNotFound", 1},
	} {
		_, failed := Failure(repository(test.status, test.reason, test.observed))
		assert.False(t, failed, "%s is not a terminal failure", test.name)
	}
}
