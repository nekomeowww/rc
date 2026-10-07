package worktreestorage

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// A Worktree clone uses a core PVC source, not an arbitrary volume populator.
const cloneSourceKind = "PersistentVolumeClaim"

// CloneSourceName recovers a committed child PVC's same-namespace source from
// its immutable dataSource/dataSourceRef fields. Use after verifying that the
// child belongs to the current Worktree incarnation; status and Repository
// observations are not authoritative once the child exists. Both references,
// when present, must describe the same core PVC. Malformed, missing, conflicting,
// or cross-namespace references return an error rather than guessing a source.
func CloneSourceName(claim *corev1.PersistentVolumeClaim) (string, error) {
	name := ""
	if source := claim.Spec.DataSource; source != nil {
		if source.Kind != cloneSourceKind || source.Name == "" || (source.APIGroup != nil && *source.APIGroup != "") {
			return "", fmt.Errorf("child PVC dataSource must reference a core PersistentVolumeClaim with a nonempty name")
		}
		name = source.Name
	}
	if source := claim.Spec.DataSourceRef; source != nil {
		if source.Kind != cloneSourceKind || source.Name == "" || (source.APIGroup != nil && *source.APIGroup != "") {
			return "", fmt.Errorf("child PVC dataSourceRef must reference a core PersistentVolumeClaim with a nonempty name")
		}
		if source.Namespace != nil && *source.Namespace != "" && *source.Namespace != claim.Namespace {
			return "", fmt.Errorf("child PVC dataSourceRef must reference its own namespace %q", claim.Namespace)
		}
		if name != "" && name != source.Name {
			return "", fmt.Errorf("child PVC dataSource and dataSourceRef reference different source claims")
		}
		name = source.Name
	}
	if name == "" {
		return "", fmt.Errorf("child PVC must record its clone source in dataSource or dataSourceRef")
	}
	return name, nil
}
