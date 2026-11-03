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

package execution

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testServiceName    = "agent"
	testServiceCommand = "device-agent"
)

func TestServicesRoundTrip(t *testing.T) {
	t.Parallel()
	services := []Service{
		{
			Name:             testServiceName,
			Command:          []string{testServiceCommand, "serve"},
			WorkingDirectory: "/workspace/app",
			Environment:      map[string]string{"MODE": "service"},
			EnvironmentFrom:  map[string]string{"TOKEN": ServiceEnvironmentPrefix + "0"},
		},
	}

	encoded, err := EncodeServices(services)
	require.NoError(t, err)
	decoded, err := DecodeServices(encoded)
	require.NoError(t, err)

	assert.Equal(t, services, decoded)
}

func TestDecodeServicesRejectsInvalidEntries(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		services []Service
	}{
		{
			name: "MissingName",
			services: []Service{
				{
					Command: []string{testServiceCommand},
				},
			},
		},
		{
			name: "MissingCommand",
			services: []Service{
				{
					Name: testServiceName,
				},
			},
		},
		{
			name: "DuplicateName",
			services: []Service{
				{
					Name:    testServiceName,
					Command: []string{testServiceCommand},
				},
				{
					Name:    testServiceName,
					Command: []string{testServiceCommand},
				},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := EncodeServices(testCase.services)
			require.NoError(t, err)
			_, err = DecodeServices(encoded)
			assert.Error(t, err)
		})
	}
}
