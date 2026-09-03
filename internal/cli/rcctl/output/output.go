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
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	prettytable "github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/yaml"
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

// Structured reports whether the selected format represents the Kubernetes object.
func (options Options) Structured() bool {
	return options.Format == FormatJSON || options.Format == FormatYAML
}

// Wide reports whether columns marked as wide should be included.
func (options Options) Wide() bool {
	return options.Format == FormatWide
}

// WriteObject restores Kubernetes type metadata and serializes an object in
// the selected structured format.
func (options Options) WriteObject(writer io.Writer, object runtime.Object, scheme *runtime.Scheme) error {
	gvk := object.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		resolved, err := apiutil.GVKForObject(object, scheme)
		if err != nil {
			return fmt.Errorf("resolve Kubernetes object type: %w", err)
		}
		object.GetObjectKind().SetGroupVersionKind(resolved)
	}
	var (
		data []byte
		err  error
	)
	switch options.Format {
	case FormatJSON:
		data, err = json.MarshalIndent(object, "", "  ")
	case FormatYAML:
		data, err = yaml.Marshal(object)
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

// ValueOrDash makes an absent scalar explicit in human-readable output.
func ValueOrDash(value string) string {
	if value == "" {
		return "-"
	}

	return value
}

// Timestamp renders a Kubernetes timestamp in a stable UTC representation.
func Timestamp(value metav1.Time) string {
	if value.IsZero() {
		return "-"
	}

	return value.UTC().Format(time.RFC3339)
}

// OptionalTimestamp renders an optional Kubernetes timestamp.
func OptionalTimestamp(value *metav1.Time) string {
	if value == nil {
		return "-"
	}

	return Timestamp(*value)
}

// Conditions summarizes Kubernetes conditions without hiding their reason or message.
func Conditions(conditions []metav1.Condition) string {
	if len(conditions) == 0 {
		return "-"
	}
	summaries := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		summary := fmt.Sprintf("%s=%s", condition.Type, condition.Status)
		if condition.Reason != "" {
			summary += " (" + condition.Reason + ")"
		}
		if condition.Message != "" {
			summary += ": " + condition.Message
		}
		summaries = append(summaries, summary)
	}

	return strings.Join(summaries, "; ")
}

// Column describes one human-readable table column.
type Column struct {
	Name     string
	MinWidth int
	MaxWidth int
	Flexible bool
	Wide     bool
}

// Table contains already-formatted values. Rows must have one value for every
// column, including columns shown only in wide output.
type Table struct {
	Columns  []Column
	Rows     [][]any
	NoHeader bool
}

// Field is one label and value in a detailed human-readable view.
type Field struct {
	Name  string
	Value any
}

// WriteDetails renders an untruncated two-column detail view.
func WriteDetails(writer io.Writer, fields []Field) error {
	rows := make([][]any, 0, len(fields))
	for _, field := range fields {
		rows = append(rows, []any{field.Name + ":", field.Value})
	}

	return WriteTable(writer, Table{
		Columns:  []Column{{Name: "FIELD"}, {Name: "VALUE"}},
		Rows:     rows,
		NoHeader: true,
	}, false)
}

// WriteTable renders a compact, borderless table. Long cells are shortened
// with an ellipsis and wide-only columns are omitted unless requested.
func WriteTable(writer io.Writer, value Table, wide bool) error {
	rendered, err := renderTable(value, wide, terminalWidth(writer))
	if err != nil {
		return err
	}
	if _, err := io.WriteString(writer, rendered+"\n"); err != nil {
		return fmt.Errorf("write table output: %w", err)
	}

	return nil
}

func renderTable(value Table, wide bool, maximumWidth int) (string, error) {
	visible := make([]int, 0, len(value.Columns))
	header := make(prettytable.Row, 0, len(value.Columns))
	for index, column := range value.Columns {
		if column.Wide && !wide {
			continue
		}
		visible = append(visible, index)
		header = append(header, column.Name)
	}

	tableWriter := prettytable.NewWriter()
	if !value.NoHeader {
		tableWriter.AppendHeader(header)
	}
	for rowIndex, row := range value.Rows {
		if len(row) != len(value.Columns) {
			return "", fmt.Errorf("table row %d has %d cells, want %d", rowIndex, len(row), len(value.Columns))
		}
		visibleRow := make(prettytable.Row, 0, len(visible))
		for _, columnIndex := range visible {
			visibleRow = append(visibleRow, normalizeCell(row[columnIndex]))
		}
		tableWriter.AppendRow(visibleRow)
	}
	tableWriter.SetColumnConfigs(tableColumnConfigs(value, visible, maximumWidth))
	tableWriter.SuppressTrailingSpaces()
	style := prettytable.StyleDefault
	style.Options = prettytable.OptionsNoBordersAndSeparators
	style.Box.PaddingLeft = ""
	style.Box.PaddingRight = "  "
	tableWriter.SetStyle(style)

	return tableWriter.Render(), nil
}

func tableColumnConfigs(value Table, visible []int, maximumWidth int) []prettytable.ColumnConfig {
	widths := make([]int, len(visible))
	minimums := make([]int, len(visible))
	flexible := make([]bool, len(visible))
	for visibleIndex, columnIndex := range visible {
		column := value.Columns[columnIndex]
		widths[visibleIndex] = text.StringWidthWithoutEscSequences(column.Name)
		for _, row := range value.Rows {
			if columnIndex >= len(row) {
				continue
			}
			cellWidth := text.StringWidthWithoutEscSequences(fmt.Sprint(normalizeCell(row[columnIndex])))
			if cellWidth > widths[visibleIndex] {
				widths[visibleIndex] = cellWidth
			}
		}
		if column.MaxWidth > 0 && widths[visibleIndex] > column.MaxWidth {
			widths[visibleIndex] = column.MaxWidth
		}
		minimums[visibleIndex] = max(column.MinWidth, text.StringWidthWithoutEscSequences(column.Name))
		if widths[visibleIndex] < minimums[visibleIndex] {
			widths[visibleIndex] = minimums[visibleIndex]
		}
		flexible[visibleIndex] = column.Flexible
	}

	shrinkFlexibleColumns(widths, minimums, flexible, maximumWidth)
	configs := make([]prettytable.ColumnConfig, len(visible))
	for index := range visible {
		configs[index] = prettytable.ColumnConfig{
			Number: index + 1, WidthMin: minimums[index], WidthMax: widths[index], WidthMaxEnforcer: snipCell,
		}
	}

	return configs
}

func shrinkFlexibleColumns(widths []int, minimums []int, flexible []bool, maximumWidth int) {
	if maximumWidth <= 0 || len(widths) == 0 {
		return
	}
	const columnSpacing = 2
	excess := columnSpacing*(len(widths)-1) - maximumWidth
	for _, width := range widths {
		excess += width
	}
	for excess > 0 {
		shrunk := false
		for index := range widths {
			if !flexible[index] || widths[index] <= minimums[index] {
				continue
			}
			widths[index]--
			excess--
			shrunk = true
			if excess == 0 {
				break
			}
		}
		if !shrunk {
			return
		}
	}
}

func normalizeCell(value any) any {
	stringValue, ok := value.(string)
	if !ok {
		return value
	}

	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(stringValue)
}

func snipCell(value string, maximum int) string {
	if maximum <= 0 || text.StringWidthWithoutEscSequences(value) <= maximum {
		return value
	}

	return text.Snip(value, maximum, "…")
}

func terminalWidth(writer io.Writer) int {
	file, ok := writer.(interface{ Fd() uintptr })
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return 0
	}

	return width
}
