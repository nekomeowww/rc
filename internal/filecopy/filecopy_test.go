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

package filecopy

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	osWindows = "windows"
	testRoot  = "root/"
)

func roundTrip(t *testing.T, source, destination string) {
	t.Helper()
	stream := new(bytes.Buffer)
	require.NoError(t, Pack(stream, source))
	require.NoError(t, Unpack(stream, destination))
}

func TestDirectoryRoundTrip(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	source := filepath.Join(t.TempDir(), "project")
	requirements.NoError(os.MkdirAll(filepath.Join(source, "nested", "deeper"), 0o755))
	payload := make([]byte, 3<<20)
	_, err := rand.Read(payload)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(filepath.Join(source, "nested", "deeper", "blob.bin"), payload, 0o644))
	requirements.NoError(os.WriteFile(filepath.Join(source, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	if runtime.GOOS != osWindows {
		requirements.NoError(os.Symlink("run.sh", filepath.Join(source, "link")))
	}

	into := t.TempDir()
	roundTrip(t, source, into)
	copied := filepath.Join(into, "project")
	data, err := os.ReadFile(filepath.Join(copied, "nested", "deeper", "blob.bin"))
	requirements.NoError(err)
	requirements.True(bytes.Equal(payload, data), "copy binary content exactly")
	if runtime.GOOS != osWindows {
		info, err := os.Stat(filepath.Join(copied, "run.sh"))
		requirements.NoError(err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "keep the executable bit")
		target, err := os.Readlink(filepath.Join(copied, "link"))
		requirements.NoError(err)
		assert.Equal(t, "run.sh", target, "keep symbolic links as links")
	}

	renamed := filepath.Join(t.TempDir(), "renamed")
	roundTrip(t, source, renamed)
	assert.FileExists(t, filepath.Join(renamed, "nested", "deeper", "blob.bin"), "a missing destination names the copy")
}

func TestFileRoundTrip(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(source, []byte("hello"), 0o600))

	into := t.TempDir()
	roundTrip(t, source, into)
	data, err := os.ReadFile(filepath.Join(into, "notes.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data), "copy into an existing directory")

	renamed := filepath.Join(t.TempDir(), "renamed.txt")
	roundTrip(t, source, renamed)
	data, err = os.ReadFile(renamed)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data), "copy to a new file name")
}

func archive(t *testing.T, headers ...tar.Header) *bytes.Buffer {
	t.Helper()
	stream := new(bytes.Buffer)
	writer := tar.NewWriter(stream)
	for _, header := range headers {
		require.NoError(t, writer.WriteHeader(&header))
		if header.Typeflag == tar.TypeReg {
			_, err := writer.Write(make([]byte, header.Size))
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	return stream
}

func TestUnpackRejectsEntriesOutsideTheRoot(t *testing.T) {
	t.Parallel()
	for name, stream := range map[string]*bytes.Buffer{
		"Parent": archive(t,
			tar.Header{Name: testRoot, Typeflag: tar.TypeDir, Mode: 0o755},
			tar.Header{Name: "root/../../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		),
		"OtherRoot": archive(t,
			tar.Header{Name: testRoot, Typeflag: tar.TypeDir, Mode: 0o755},
			tar.Header{Name: "other/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			require.Error(t, Unpack(stream, filepath.Join(parent, "out")))
			assert.NoFileExists(t, filepath.Join(filepath.Dir(parent), "escape"))
		})
	}
}

func TestUnpackNeverWritesThroughAnEscapingLink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("creating symbolic links needs privileges on Windows")
	}
	outside := t.TempDir()
	stream := archive(t,
		tar.Header{Name: testRoot, Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "root/link", Typeflag: tar.TypeSymlink, Linkname: outside},
		tar.Header{Name: "root/link/evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
	)
	require.Error(t, Unpack(stream, filepath.Join(t.TempDir(), "out")))
	assert.NoFileExists(t, filepath.Join(outside, "evil"))
}
