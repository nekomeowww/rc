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

package workspaces

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

const testNPMRegistryURL = "https://registry.example.com"
const testNPMRegistryPathURL = "https://packages.example.com/npm"

func TestNPMRegistryEnvironmentNormalizesURLs(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		input    string
		npm      string
		corepack string
	}{
		{name: "HTTP", input: "http://verdaccio.rc-system.svc.cluster.local:4873", npm: "http://verdaccio.rc-system.svc.cluster.local:4873/", corepack: "http://verdaccio.rc-system.svc.cluster.local:4873"},
		{name: "HTTPSWithSlash", input: testNPMRegistryURL + "/", npm: testNPMRegistryURL + "/", corepack: testNPMRegistryURL},
		{name: "RegistryPathWithoutSlash", input: testNPMRegistryPathURL, npm: testNPMRegistryPathURL + "/", corepack: testNPMRegistryPathURL},
		{name: "RegistryPath", input: testNPMRegistryPathURL + "/", npm: testNPMRegistryPathURL + "/", corepack: testNPMRegistryPathURL},
		{name: "EscapedRegistryPath", input: "https://packages.example.com/npm%2Fprivate/", npm: "https://packages.example.com/npm%2Fprivate/", corepack: "https://packages.example.com/npm%2Fprivate"},
		{name: "RepeatedTrailingSlashes", input: "https://packages.example.com/npm///", npm: "https://packages.example.com/npm/", corepack: "https://packages.example.com/npm"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			environment, err := NPMRegistryEnvironment(testCase.input)
			require.NoError(t, err, "normalize a valid npm registry URL")
			require.Len(t, environment, 2, "expand both registry environment variables")
			assert.Equal(t, corev1.EnvVar{Name: NPMRegistryEnvironmentName, Value: testCase.npm}, environment[0])
			assert.Equal(t, corev1.EnvVar{Name: CorepackRegistryEnvironmentName, Value: testCase.corepack}, environment[1])
		})
	}
}

func TestNPMRegistryEnvironmentRejectsUnsafeURLs(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		input string
		error string
	}{
		{name: "Scheme", input: "ftp://registry.example.com", error: "parse --npm-registry: scheme must be http or https"},
		{name: "MissingHost", input: "https:///npm", error: "parse --npm-registry: host is required"},
		{name: "PortWithoutHost", input: "https://:4873/npm", error: "parse --npm-registry: host is required"},
		{name: "Userinfo", input: "https://user:password@registry.example.com", error: "parse --npm-registry: userinfo is not allowed"},
		{name: "Query", input: "https://registry.example.com/npm?channel=next", error: "parse --npm-registry: query is not allowed"},
		{name: "EmptyQuery", input: "https://registry.example.com/npm?", error: "parse --npm-registry: query is not allowed"},
		{name: "Fragment", input: "https://registry.example.com/npm#section", error: "parse --npm-registry: fragment is not allowed"},
		{name: "EmptyFragment", input: "https://registry.example.com/npm#", error: "parse --npm-registry: fragment is not allowed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := NPMRegistryEnvironment(testCase.input)
			require.Error(t, err, "reject an unsafe npm registry URL")
			assert.EqualError(t, err, testCase.error)
		})
	}
}

func TestNPMRegistryEnvironmentEmptyValueDoesNothing(t *testing.T) {
	t.Parallel()
	environment, err := NPMRegistryEnvironment("")
	require.NoError(t, err)
	assert.Nil(t, environment, "omit registry variables when the flag is empty")
}

func TestValidateNPMRegistryEnvironmentConflictsRejectsExplicitValues(t *testing.T) {
	t.Parallel()
	registry, err := NPMRegistryEnvironment(testNPMRegistryURL)
	require.NoError(t, err)

	t.Run("EnvNPM", func(t *testing.T) {
		err := ValidateNPMRegistryEnvironmentConflicts(nil, []string{NPMRegistryEnvironmentName + "=other"}, registry)
		require.Error(t, err)
		assert.EqualError(t, err, "--npm-registry conflicts with --env value "+NPMRegistryEnvironmentName)
	})
	t.Run("EnvCorepack", func(t *testing.T) {
		err := ValidateNPMRegistryEnvironmentConflicts(nil, []string{CorepackRegistryEnvironmentName + "=other"}, registry)
		require.Error(t, err)
		assert.EqualError(t, err, "--npm-registry conflicts with --env value "+CorepackRegistryEnvironmentName)
	})
	t.Run("EnvFile", func(t *testing.T) {
		err := ValidateNPMRegistryEnvironmentConflicts([]map[string]string{{NPMRegistryEnvironmentName: "other"}}, nil, registry)
		require.Error(t, err)
		assert.EqualError(t, err, "--npm-registry conflicts with --env-file value "+NPMRegistryEnvironmentName)
	})
	t.Run("CaseDistinct", func(t *testing.T) {
		err := ValidateNPMRegistryEnvironmentConflicts(nil, []string{"npm_config_registry=other"}, registry)
		require.NoError(t, err, "preserve case-sensitive environment names for non-Windows targets")
	})
}

func TestAddNPMRegistryEnvironmentProducesUniqueNames(t *testing.T) {
	t.Parallel()
	registry, err := NPMRegistryEnvironment(testNPMRegistryURL)
	require.NoError(t, err)
	values := map[string]string{NPMRegistryEnvironmentName: "caller-value"}
	AddNPMRegistryEnvironment(values, registry)
	assert.Equal(t, map[string]string{
		NPMRegistryEnvironmentName:      testNPMRegistryURL + "/",
		CorepackRegistryEnvironmentName: testNPMRegistryURL,
	}, values, "override caller pass-through without duplicate names")
}
