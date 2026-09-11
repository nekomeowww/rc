package command

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PlacementOptions shares explicit runtime OS and scheduling flags across
// Workspace, Environment, and temporary process creation.
type PlacementOptions struct {
	OS           string
	NodeSelector map[string]string
	Tolerations  []string
}

func (options *PlacementOptions) AddFlags(flags *pflag.FlagSet) {
	flags.StringVar(&options.OS, "os", "", "Runtime OS: linux, windows, or darwin; defaults to the Environment OS or linux")
	flags.StringToStringVar(&options.NodeSelector, "node-selector", nil, "Runtime node labels (key=value)")
	flags.StringArrayVar(&options.Tolerations, "toleration", nil, "Runtime taint to tolerate (key[=value]:effect); repeatable")
}

func (options PlacementOptions) Resolve() (corev1.OSName, []corev1.Toleration, error) {
	if options.OS != "" && options.OS != "linux" && options.OS != "windows" && options.OS != "darwin" {
		return "", nil, fmt.Errorf("--os must be linux, windows, or darwin")
	}
	result := make([]corev1.Toleration, 0, len(options.Tolerations))
	for _, item := range options.Tolerations {
		keyValue, effect, ok := strings.Cut(item, ":")
		if !ok || (effect != string(corev1.TaintEffectNoSchedule) && effect != string(corev1.TaintEffectPreferNoSchedule) && effect != string(corev1.TaintEffectNoExecute)) {
			return "", nil, fmt.Errorf("invalid toleration %q: use key[=value]:NoSchedule, PreferNoSchedule, or NoExecute", item)
		}
		key, value, equal := strings.Cut(keyValue, "=")
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return "", nil, fmt.Errorf("invalid toleration key or value in %q", item)
		}
		operator := corev1.TolerationOpExists
		if equal {
			operator = corev1.TolerationOpEqual
		}
		result = append(result, corev1.Toleration{Key: key, Value: value, Operator: operator, Effect: corev1.TaintEffect(effect)})
	}
	return corev1.OSName(options.OS), result, nil
}
