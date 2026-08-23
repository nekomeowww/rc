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

package command

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	defaultGPUResource     = corev1.ResourceName("nvidia.com/gpu")
	defaultGPUVRAMResource = corev1.ResourceName("nvidia.com/gpumem")
)

// GPUOptions contains the portable GPU flags shared by Workspace commands.
type GPUOptions struct {
	Count     uint32
	VRAM      string
	Resources []string
}

// AddFlags adds high-level GPU requests and the low-level extended-resource
// escape hatch to flags.
func (options *GPUOptions) AddFlags(flags *pflag.FlagSet) {
	flags.Uint32Var(&options.Count, "gpu", 0, "GPU count (uses nvidia.com/gpu)")
	flags.StringVar(&options.VRAM, "gpu-vram", "", "GPU VRAM quantity (uses nvidia.com/gpumem)")
	flags.StringArrayVar(&options.Resources, "gpu-resource", nil, "Low-level Kubernetes extended resource NAME=QUANTITY; repeat")
}

// ResourceRequirements converts GPU flags into the requests and limits of a
// Workspace runtime container. gpuSet and vramSet distinguish an omitted flag
// from an explicitly supplied empty or zero value.
func (options GPUOptions) ResourceRequirements(gpuSet bool, vramSet bool) (corev1.ResourceRequirements, error) {
	resources := make(corev1.ResourceList)
	if gpuSet {
		if options.Count == 0 {
			return corev1.ResourceRequirements{}, fmt.Errorf("--gpu must be greater than zero")
		}
		resources[defaultGPUResource] = *resource.NewQuantity(int64(options.Count), resource.DecimalSI)
	}
	if vramSet {
		quantity, err := positiveWholeQuantity("--gpu-vram", options.VRAM)
		if err != nil {
			return corev1.ResourceRequirements{}, err
		}
		resources[defaultGPUVRAMResource] = quantity
	}
	for _, value := range options.Resources {
		name, encodedQuantity, found := strings.Cut(value, "=")
		if !found || name == "" || encodedQuantity == "" {
			return corev1.ResourceRequirements{}, fmt.Errorf("parse --gpu-resource %q: expected NAME=QUANTITY", value)
		}
		if err := validateExtendedResourceName(name); err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("parse --gpu-resource %q: %w", value, err)
		}
		resourceName := corev1.ResourceName(name)
		if _, exists := resources[resourceName]; exists {
			return corev1.ResourceRequirements{}, fmt.Errorf("GPU resource %q is selected more than once", name)
		}
		quantity, err := positiveWholeQuantity("--gpu-resource "+name, encodedQuantity)
		if err != nil {
			return corev1.ResourceRequirements{}, err
		}
		resources[resourceName] = quantity
	}
	if len(resources) == 0 {
		return corev1.ResourceRequirements{}, nil
	}

	return corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources}, nil
}

func positiveWholeQuantity(flag string, value string) (resource.Quantity, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil || quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("parse %s: value must be a positive Kubernetes quantity", flag)
	}
	if _, exact := quantity.AsInt64(); !exact {
		return resource.Quantity{}, fmt.Errorf("parse %s: extended resource quantity must be a whole number", flag)
	}

	return quantity, nil
}

func validateExtendedResourceName(name string) error {
	if problems := validation.IsQualifiedName(name); len(problems) > 0 {
		return fmt.Errorf("invalid Kubernetes resource name: %s", strings.Join(problems, ", "))
	}
	namespace, _, qualified := strings.Cut(name, "/")
	if !qualified || namespace == "kubernetes.io" || strings.HasSuffix(namespace, ".kubernetes.io") {
		return fmt.Errorf("resource name must use a non-Kubernetes qualified domain")
	}

	return nil
}
