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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestGPUOptionsBuildsHighAndLowLevelResources(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	options := GPUOptions{
		Count: 2, VRAM: "24Gi",
		Resources: []string{"amd.com/gpu=1", "example.com/gpu-slices=4"},
	}

	result, err := options.ResourceRequirements(true, true)
	requirements.NoError(err, "build GPU resource requirements")
	requirements.Len(result.Requests, 4, "include every requested GPU resource")
	requirements.Len(result.Limits, 4, "limit every requested GPU resource")

	want := corev1.ResourceList{
		defaultGPUResource:                            resource.MustParse("2"),
		defaultGPUVRAMResource:                        resource.MustParse("24Gi"),
		corev1.ResourceName("amd.com/gpu"):            resource.MustParse("1"),
		corev1.ResourceName("example.com/gpu-slices"): resource.MustParse("4"),
	}
	for name, wantQuantity := range want {
		requestQuantity := result.Requests[name]
		limitQuantity := result.Limits[name]
		assertions.Zero(wantQuantity.Cmp(requestQuantity), "translate %s into a request", name)
		assertions.Zero(wantQuantity.Cmp(limitQuantity), "translate %s into a limit", name)
	}
}

func TestGPUOptionsAllowsVRAMWithoutGPUCountOrRawResources(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)

	result, err := (GPUOptions{VRAM: "16Gi"}).ResourceRequirements(false, true)
	requirements.NoError(err, "build a VRAM-only request")
	requirements.Len(result.Limits, 1, "request only the default VRAM resource")
	assertions.Equal(resource.MustParse("16Gi"), result.Limits[defaultGPUVRAMResource], "retain the requested VRAM quantity")
}

func TestGPUOptionsRejectsInvalidResources(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		options GPUOptions
		gpuSet  bool
		vramSet bool
		want    string
	}{
		"ZeroGPUCount":       {options: GPUOptions{}, gpuSet: true, want: "--gpu must be greater than zero"},
		"InvalidVRAM":        {options: GPUOptions{VRAM: "nope"}, vramSet: true, want: "parse --gpu-vram: value must be a positive Kubernetes quantity"},
		"FractionalVRAM":     {options: GPUOptions{VRAM: "1500m"}, vramSet: true, want: "parse --gpu-vram: extended resource quantity must be a whole number"},
		"MissingAssignment":  {options: GPUOptions{Resources: []string{"amd.com/gpu"}}, want: "parse --gpu-resource \"amd.com/gpu\": expected NAME=QUANTITY"},
		"NativeResource":     {options: GPUOptions{Resources: []string{"cpu=1"}}, want: "parse --gpu-resource \"cpu=1\": resource name must use a non-Kubernetes qualified domain"},
		"DuplicateDefault":   {options: GPUOptions{Count: 1, Resources: []string{"nvidia.com/gpu=2"}}, gpuSet: true, want: "GPU resource \"nvidia.com/gpu\" is selected more than once"},
		"FractionalResource": {options: GPUOptions{Resources: []string{"example.com/gpu=500m"}}, want: "parse --gpu-resource example.com/gpu: extended resource quantity must be a whole number"},
	}
	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := testCase.options.ResourceRequirements(testCase.gpuSet, testCase.vramSet)
			require.Error(t, err, "reject an invalid GPU request")
			assert.EqualError(t, err, testCase.want, "report the invalid GPU request")
		})
	}
}
