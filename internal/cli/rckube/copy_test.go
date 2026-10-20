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

package rckube

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/filecopy"
)

type countingReader struct {
	reader io.Reader
	read   int
}

func (counting *countingReader) Read(data []byte) (int, error) {
	count, err := counting.reader.Read(data)
	counting.read += count
	return count, err
}

func TestCopyToDrainsInputWhenUnpackFails(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "payload")
	require.NoError(t, os.WriteFile(source, bytes.Repeat([]byte("x"), 1<<20), 0o644))
	stream := new(bytes.Buffer)
	require.NoError(t, filecopy.Pack(stream, source))
	total := stream.Len()
	input := &countingReader{reader: stream}

	command := newCopyToCommand()
	command.SetIn(input)
	command.SetArgs([]string{filepath.Join(t.TempDir(), "missing-parent", "payload")})
	command.SilenceUsage, command.SilenceErrors = true, true
	require.Error(t, command.Execute(), "report the destination error")
	require.Equal(t, total, input.read, "consume the whole stream so the sender can finish")
}
