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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	repositories "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

// FailedReason is the consumer reason for a Repository that cannot become
// ready without a user action.
const FailedReason = "RepositoryFailed"

// failedStorageReasons are StorageReady=False reasons that wait for a user
// action: a new RepositorySync, a Credential, or a spec fix. Other False
// reasons describe provisioning, bootstrap, sync, or expansion in progress.
var failedStorageReasons = map[string]struct{}{
	"SyncFailed":                    {},
	"BootstrapFailed":               {},
	"BootstrapJobConflict":          {},
	"CredentialNotFound":            {},
	"VolumeClaimConflict":           {},
	"StorageClassChangeUnsupported": {},
	"VolumeShrinkUnsupported":       {},
}

// Failure reports why a Repository's parent volume cannot become ready on its
// own. Consumers fail fast with this message instead of waiting. A condition
// from an earlier generation is not a failure: the controller has not yet
// observed the spec that may fix it.
func Failure(repository *repositories.Repository) (string, bool) {
	condition := meta.FindStatusCondition(repository.Status.Conditions, repositories.RepositoryConditionStorageReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.ObservedGeneration != repository.Generation ||
		repository.Status.ObservedGeneration != repository.Generation {
		return "", false
	}
	if _, failed := failedStorageReasons[condition.Reason]; !failed {
		return "", false
	}

	return fmt.Sprintf("Repository %s is not usable (%s): %s", repository.Name, condition.Reason, condition.Message), true
}
