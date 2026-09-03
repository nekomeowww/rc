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

// Package output renders CLI results in human-readable and structured formats.
package output

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	FormatTable = "table"
	FormatWide  = "wide"
	FormatJSON  = "json"
	FormatYAML  = "yaml"
)

// Options selects the human-readable or structured representation of a command result.
type Options struct {
	Format string
}

// AddFlags adds the common output format flag to a command.
func (options *Options) AddFlags(command *cobra.Command, allowWide bool) {
	formats := "table, json, or yaml"
	if allowWide {
		formats = "table, wide, json, or yaml"
	}
	command.Flags().StringVarP(&options.Format, "output", "o", "", "Output format: "+formats)
}

// Validate rejects unsupported output formats. An empty format selects the
// command's default human-readable representation.
func (options Options) Validate(allowWide bool) error {
	switch options.Format {
	case "", FormatTable, FormatJSON, FormatYAML:
		return nil
	case FormatWide:
		if allowWide {
			return nil
		}
	}

	return fmt.Errorf("unsupported output format %q", options.Format)
}

// PrintList renders a list as a compact table, a wide table, or the original
// Kubernetes object in a structured format.
func (options Options) PrintList(writer io.Writer, object runtime.Object, scheme *runtime.Scheme, table Table) error {
	if err := options.Validate(true); err != nil {
		return err
	}
	if options.isStructured() {
		return options.writeObject(writer, object, scheme)
	}

	return writeTable(writer, table, options.Format == FormatWide)
}

// PrintDetails renders a detailed human-readable view or the original
// Kubernetes object in a structured format.
func (options Options) PrintDetails(
	writer io.Writer,
	object runtime.Object,
	scheme *runtime.Scheme,
	fields []Field,
) error {
	if err := options.Validate(false); err != nil {
		return err
	}
	if options.isStructured() {
		return options.writeObject(writer, object, scheme)
	}

	return writeDetails(writer, fields)
}

func (options Options) isStructured() bool {
	return options.Format == FormatJSON || options.Format == FormatYAML
}

func (options Options) writeObject(writer io.Writer, object runtime.Object, scheme *runtime.Scheme) error {
	if err := restoreTypeMetadata(object, scheme); err != nil {
		return err
	}

	var (
		data []byte
		err  error
	)
	switch options.Format {
	case FormatJSON:
		data, err = marshalJSON(object)
	case FormatYAML:
		data, err = marshalYAML(object)
	default:
		return fmt.Errorf("output format %q is not structured", options.Format)
	}
	if err != nil {
		return fmt.Errorf("marshal %s output: %w", options.Format, err)
	}
	terminated := make([]byte, 0, len(data)+1)
	terminated = append(terminated, data...)
	terminated = append(terminated, '\n')
	if _, err := writer.Write(terminated); err != nil {
		return fmt.Errorf("write %s output: %w", options.Format, err)
	}

	return nil
}
