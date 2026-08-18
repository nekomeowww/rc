package repositories

import (
	"crypto/sha256"
	"fmt"
	"strings"

	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
)

// WorktreeName derives a stable Kubernetes name when the caller does not
// provide one. Branch/ref names remain readable; nameless detached worktrees
// receive a short random suffix because they have no stable identity.
func WorktreeName(repositoryName, branch, ref string, detach, orphan bool) string {
	base := repositoryName

	switch {
	case branch != "":
		base += "--" + branch
	case ref != "":
		base += "--" + ref
	case detach:
		base += "--detached-" + utilrand.String(6)
	case orphan:
		base += "--orphan-" + utilrand.String(6)
	default:
		base += "--worktree-" + utilrand.String(6)
	}

	return normalizeWorktreeName(base)
}

func normalizeWorktreeName(value string) string {
	var builder strings.Builder
	previousDash := false
	for _, character := range strings.ToLower(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			builder.WriteRune(character)
			previousDash = false
			continue
		}
		if !previousDash {
			builder.WriteByte('-')
			previousDash = true
		}
	}

	name := strings.Trim(builder.String(), "-")
	if name == "" {
		name = "worktree-" + utilrand.String(6)
	}
	if len(name) > 253 {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(value)))[:12]
		name = strings.Trim(name[:253-len(digest)-1], "-") + "-" + digest
	}
	if errors := validation.IsDNS1123Subdomain(name); len(errors) > 0 {
		return "worktree-" + utilrand.String(6)
	}

	return name
}
