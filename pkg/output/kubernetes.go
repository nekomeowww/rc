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

package output

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

func restoreTypeMetadata(object runtime.Object, scheme *runtime.Scheme) error {
	if !object.GetObjectKind().GroupVersionKind().Empty() {
		return nil
	}
	gvk, err := apiutil.GVKForObject(object, scheme)
	if err != nil {
		return fmt.Errorf("resolve Kubernetes object type: %w", err)
	}
	object.GetObjectKind().SetGroupVersionKind(gvk)

	return nil
}

// ValueOrDash makes an absent scalar explicit in human-readable output.
func ValueOrDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}

// Timestamp renders a Kubernetes timestamp in a stable UTC representation.
func Timestamp(value metav1.Time) string {
	if value.IsZero() {
		return "-"
	}

	return value.UTC().Format(time.RFC3339)
}

// OptionalTimestamp renders an optional Kubernetes timestamp.
func OptionalTimestamp(value *metav1.Time) string {
	if value == nil {
		return "-"
	}

	return Timestamp(*value)
}

// Conditions summarizes Kubernetes conditions without hiding their reason or message.
func Conditions(conditions []metav1.Condition) string {
	if len(conditions) == 0 {
		return "-"
	}
	summaries := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		summary := fmt.Sprintf("%s=%s", condition.Type, condition.Status)
		if condition.Reason != "" {
			summary += " (" + condition.Reason + ")"
		}
		if condition.Message != "" {
			summary += ": " + condition.Message
		}
		summaries = append(summaries, summary)
	}

	return strings.Join(summaries, "; ")
}
