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

package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestOptionsValidateSupportedFormats(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"", FormatTable, FormatJSON, FormatYAML} {
		assert.NoError(t, (Options{Format: format}).Validate(false), "accept %q for every command", format)
	}
	assert.NoError(t, (Options{Format: FormatWide}).Validate(true), "accept wide output for list commands")
	err := (Options{Format: FormatWide}).Validate(false)
	require.Error(t, err, "reject wide output for detail commands")
	assert.EqualError(t, err, `unsupported output format "wide"`)
}

func TestPrintDetailsDispatchesStructuredOutputAndRestoresTypeMetadata(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme), "register core Kubernetes types")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "example"}}
	var output bytes.Buffer

	err := (Options{Format: FormatJSON}).PrintDetails(&output, pod, scheme, nil)
	require.NoError(t, err, "print a registered Kubernetes object")

	assert.Contains(t, output.String(), `"apiVersion": "v1"`, "include the resolved API version")
	assert.Contains(t, output.String(), `"kind": "Pod"`, "include the resolved object kind")
}

func TestPrintListDispatchesWideTableOutput(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer

	err := (Options{Format: FormatWide}).PrintList(&output, nil, nil, Table{
		Columns: []Column{{Name: testNameHeader}, {Name: "DETAIL", Wide: true}},
		Rows:    [][]any{{"example", "visible"}},
	})
	require.NoError(t, err, "print a wide table")

	assert.Contains(t, output.String(), "DETAIL", "dispatch wide output to the table formatter")
	assert.Contains(t, output.String(), "visible", "include wide-only values")
}
