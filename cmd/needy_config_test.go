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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func TestNeedyConfigurationParsesKVMPlacement(t *testing.T) {
	config, err := needyConfiguration("example.com/needy:1", "example.com/kvm", "example.com/kvm=true,zone=a",
		`[{"key":"example.com/kvm","operator":"Equal","value":"true","effect":"NoSchedule"}]`,
		104, false, "", "10.0.0.0/8")
	require.NoError(t, err)
	assert.Equal(t, labels.Set{
		"example.com/kvm": "true",
		"zone":            "a",
	}, config.KVMNodeSelector)
	assert.Equal(t, []corev1.Toleration{
		{
			Key:      "example.com/kvm",
			Operator: corev1.TolerationOpEqual,
			Value:    "true",
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}, config.KVMTolerations)
	assert.Equal(t, []string{"10.0.0.0/8"}, config.GatewayBlockCIDRs)

	_, err = needyConfiguration("", "", "missing-value", "", 0, false, "", "")
	require.Error(t, err)
	_, err = needyConfiguration("", "", "", `[{"key":"a","unknown":1}]`, 0, false, "", "")
	require.Error(t, err)
	_, err = needyConfiguration("", "", "", "", 0, false, "", "not-a-cidr")
	require.Error(t, err)
}
