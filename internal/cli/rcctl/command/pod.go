package command

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
)

// RuntimePodOptions holds flags that apply only to the Workspace runtime Pod:
// extra labels and annotations, and its PriorityClass.
type RuntimePodOptions struct {
	Labels        []string
	Annotations   []string
	PriorityClass string
}

// AddFlags exposes runtime Pod metadata and priority for Workspace creation.
func (options *RuntimePodOptions) AddFlags(flags *pflag.FlagSet) {
	flags.StringArrayVar(&options.Labels, "pod-label", nil, "Runtime Pod label (key=value); repeatable; rc.ayaka.io keys are reserved")
	flags.StringArrayVar(&options.Annotations, "pod-annotation", nil, "Runtime Pod annotation (key=value); repeatable; rc.ayaka.io keys are reserved")
	flags.StringVar(&options.PriorityClass, "priority-class", "", "PriorityClass for the runtime Pod")
}

// Resolve validates the flags before connecting to the cluster. It returns nil
// metadata when no label or annotation was given.
func (options RuntimePodOptions) Resolve() (*workspacesv1alpha1.WorkspacePodMetadata, string, error) {
	labels, err := parsePodMetadataPairs("--pod-label", options.Labels)
	if err != nil {
		return nil, "", err
	}
	annotations, err := parsePodMetadataPairs("--pod-annotation", options.Annotations)
	if err != nil {
		return nil, "", err
	}
	if errs := metav1validation.ValidateLabels(labels, field.NewPath("--pod-label")); len(errs) > 0 {
		return nil, "", errs.ToAggregate()
	}
	if errs := apivalidation.ValidateAnnotations(annotations, field.NewPath("--pod-annotation")); len(errs) > 0 {
		return nil, "", errs.ToAggregate()
	}
	if options.PriorityClass != "" {
		if problems := validation.IsDNS1123Subdomain(options.PriorityClass); len(problems) > 0 {
			return nil, "", fmt.Errorf("invalid --priority-class %q: %s", options.PriorityClass, strings.Join(problems, "; "))
		}
	}
	if len(labels) == 0 && len(annotations) == 0 {
		return nil, options.PriorityClass, nil
	}

	metadata := new(workspacesv1alpha1.WorkspacePodMetadata)
	if len(labels) > 0 {
		metadata.Labels = labels
	}
	if len(annotations) > 0 {
		metadata.Annotations = annotations
	}

	return metadata, options.PriorityClass, nil
}

func parsePodMetadataPairs(flag string, items []string) (map[string]string, error) {
	result := make(map[string]string, len(items))
	for _, item := range items {
		key, value, found := strings.Cut(item, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("invalid %s %q: use key=value", flag, item)
		}
		if workspacesv1alpha1.IsReservedPodMetadataKey(key) {
			return nil, fmt.Errorf("invalid %s %q: keys with an %s prefix are reserved for rc", flag, item, workspacesv1alpha1.ReservedPodMetadataDomain)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%s key %q is given more than once", flag, key)
		}
		result[key] = value
	}

	return result, nil
}
