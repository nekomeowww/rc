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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCommandHeader = "COMMAND"
	testNameHeader    = "NAME"
)

func TestRenderTableSelectsColumnsAndSnipsLongCells(t *testing.T) {
	t.Parallel()
	value := Table{
		Columns: []Column{
			{Name: testNameHeader},
			{Name: testCommandHeader, MaxWidth: 8},
			{Name: "CLIENTS", Wide: true},
		},
		Rows: [][]any{{"process", "运行很长的命令", 2}},
	}

	compact, err := renderTable(value, false, 0)
	require.NoError(t, err, "render compact table")
	wide, err := renderTable(value, true, 0)
	require.NoError(t, err, "render wide table")

	assert.NotContains(t, compact, "CLIENTS", "omit wide-only columns from compact output")
	assert.Contains(t, compact, "运行很…", "snip a wide-character cell by display width")
	assert.Contains(t, wide, "CLIENTS", "include wide-only columns in wide output")
}

func TestRenderTableNormalizesCellBreaks(t *testing.T) {
	t.Parallel()
	rendered, err := renderTable(Table{
		Columns: []Column{{Name: testCommandHeader}},
		Rows:    [][]any{{"printf\tfirst\nsecond"}},
	}, false, 0)
	require.NoError(t, err, "render table containing control characters")

	assert.Contains(t, rendered, "printf first second", "keep one physical line per table row")
}

func TestRenderTableRejectsMismatchedRows(t *testing.T) {
	t.Parallel()
	_, err := renderTable(Table{
		Columns: []Column{{Name: testNameHeader}, {Name: "AGE"}},
		Rows:    [][]any{{"process"}},
	}, false, 0)
	require.Error(t, err, "reject a row with missing cells")
	assert.EqualError(t, err, "table row 0 has 1 cells, want 2")
}

func TestSnipCellLeavesShortValuesUnpadded(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "short", snipCell("short", 20), "do not pad a value to its maximum width")
	assert.Equal(t, 8, len([]rune(snipCell(strings.Repeat("x", 20), 8))), "include the ellipsis within the maximum width")
}

func TestRenderTableFitsTerminalWidthWithoutDroppingTrailingColumns(t *testing.T) {
	t.Parallel()
	rendered, err := renderTable(Table{
		Columns: []Column{
			{Name: "ID", MaxWidth: 20}, {Name: testCommandHeader, MinWidth: 8, MaxWidth: 48, Flexible: true},
			{Name: "PHASE"}, {Name: "AGE"}, {Name: "EXIT"},
		},
		Rows: [][]any{{"process-1234567890", strings.Repeat("command ", 20), "Succeeded", "24h", "137"}},
	}, false, 60)
	require.NoError(t, err, "render a terminal-width table")

	for line := range strings.SplitSeq(rendered, "\n") {
		assert.LessOrEqual(t, len([]rune(line)), 60, "fit every rendered line within the terminal")
	}
	assert.Contains(t, rendered, "137", "preserve the trailing exit-code column")
}
