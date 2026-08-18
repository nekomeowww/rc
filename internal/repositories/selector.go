package repositories

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
)

type repositorySelector struct {
	host    string
	path    string
	hasHost bool
}

// ResolveRepository accepts a resource name, host/path, or a full Git URL.
// Name matching is exact first; path-only matching is allowed only when it is
// unambiguous in the namespace.
func ResolveRepository(ctx context.Context, kubeClient client.Client, namespace, input string) (*repositoriesv1alpha1.Repository, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("repository selector must not be empty")
	}

	byName := new(repositoriesv1alpha1.Repository)
	if len(validation.IsDNS1123Subdomain(input)) == 0 {
		if err := kubeClient.Get(ctx, types.NamespacedName{Name: input, Namespace: namespace}, byName); err == nil {
			return byName, nil
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get Repository %q: %w", input, err)
		}
	}

	selector, err := parseRepositorySelector(input)
	if err != nil {
		return nil, err
	}

	list := new(repositoriesv1alpha1.RepositoryList)
	if err := kubeClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Repositories while resolving %q: %w", input, err)
	}

	matches := make([]repositoriesv1alpha1.Repository, 0)
	for index := range list.Items {
		candidate := &list.Items[index]

		candidateSelector, candidateErr := parseRepositorySelector(candidate.Spec.Remote.URL)
		if candidateErr != nil {
			continue
		}

		if selector.host != "" {
			if candidateSelector.host == selector.host && candidateSelector.path == selector.path {
				matches = append(matches, *candidate)
			}
		} else if candidateSelector.path == selector.path {
			matches = append(matches, *candidate)
		}
	}

	if len(matches) == 1 {
		return &matches[0], nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Name)
		}
		slices.Sort(names)

		return nil, fmt.Errorf("repository selector %q is ambiguous; matches %s", input, strings.Join(names, ", "))
	}

	return nil, fmt.Errorf("repository %q was not found in namespace %q", input, namespace)
}

func parseRepositorySelector(input string) (repositorySelector, error) {
	input = strings.TrimSpace(input)
	if sshInput, ok := strings.CutPrefix(input, "git@"); ok {
		parts := strings.SplitN(sshInput, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return repositorySelector{}, fmt.Errorf("invalid Git SSH selector %q", input)
		}
		return repositorySelector{host: strings.ToLower(parts[0]), path: normalizeRepositoryPath(parts[1]), hasHost: true}, nil
	}
	if strings.Contains(input, "://") {
		parsed, err := url.Parse(input)
		if err != nil || parsed.Hostname() == "" || parsed.Path == "" {
			return repositorySelector{}, fmt.Errorf("invalid Git URL selector %q", input)
		}
		return repositorySelector{host: strings.ToLower(parsed.Hostname()), path: normalizeRepositoryPath(parsed.Path), hasHost: true}, nil
	}

	parts := strings.Split(strings.Trim(input, "/"), "/")
	if len(parts) >= 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		return repositorySelector{host: strings.ToLower(parts[0]), path: normalizeRepositoryPath(strings.Join(parts[1:], "/")), hasHost: true}, nil
	}

	return repositorySelector{path: normalizeRepositoryPath(input)}, nil
}

func normalizeRepositoryPath(path string) string {
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")

	return path
}
