/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package command

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ParseAccessModes validates PersistentVolumeClaim access modes supplied by a
// repeatable StringSlice flag.
func ParseAccessModes(values []string) ([]corev1.PersistentVolumeAccessMode, error) {
	if len(values) == 0 {
		return nil, nil
	}

	accessModes := make([]corev1.PersistentVolumeAccessMode, 0, len(values))
	for _, value := range values {
		mode := corev1.PersistentVolumeAccessMode(value)
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadOnlyMany, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
			accessModes = append(accessModes, mode)
		default:
			return nil, fmt.Errorf("unsupported --access-mode %q", value)
		}
	}

	return accessModes, nil
}
