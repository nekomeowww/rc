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
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeExecutable(t *testing.T, name, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o755))
	require.NoError(t, os.WriteFile(name, []byte(content), 0o755))

	return name
}

func TestInstallCopiesRCKubeAndTini(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	executable := writeExecutable(t, filepath.Join(source, "rc-kube"), "rc-kube binary")
	writeExecutable(t, filepath.Join(source, TiniName), "tini binary")
	target := t.TempDir()

	require.NoError(t, Install(executable, target))
	// The init container runs again after a Pod sandbox restart.
	require.NoError(t, Install(executable, target), "install again over the earlier copies")

	for name, content := range map[string]string{
		"rc-kube": "rc-kube binary",
		TiniName:  "tini binary",
	} {
		data, err := os.ReadFile(filepath.Join(target, name))
		require.NoError(t, err)
		assert.Equal(t, content, string(data))
		if goruntime.GOOS != runtimeOSWindows {
			info, err := os.Stat(filepath.Join(target, name))
			require.NoError(t, err)
			assert.Equal(t, installedMode, info.Mode().Perm(), "every user can run %s", name)
		}
	}
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "leave no temporary files")
}

func TestInstallRequiresTini(t *testing.T) {
	t.Parallel()
	executable := writeExecutable(t, filepath.Join(t.TempDir(), "rc-kube"), "rc-kube binary")

	err := Install(executable, t.TempDir())
	require.ErrorContains(t, err, TiniName, "an rc-kube image without tini cannot start a runtime")
}

func TestFindTiniPrefersTheTiniNextToRCKube(t *testing.T) {
	if goruntime.GOOS == runtimeOSWindows {
		t.Skip("tini runs only on Linux")
	}
	directory := t.TempDir()
	executable := writeExecutable(t, filepath.Join(directory, "rc-kube"), "rc-kube binary")
	sibling := writeExecutable(t, filepath.Join(directory, TiniName), "tini binary")
	onPath := writeExecutable(t, filepath.Join(t.TempDir(), TiniName), "image tini")
	t.Setenv("PATH", filepath.Dir(onPath))

	found, err := FindTini(executable)
	require.NoError(t, err)
	assert.Equal(t, sibling, found, "use the injected tini")

	require.NoError(t, os.Remove(sibling))
	found, err = FindTini(executable)
	require.NoError(t, err)
	assert.Equal(t, onPath, found, "an rc-kube in an image uses the tini of the image")

	t.Setenv("PATH", t.TempDir())
	_, err = FindTini(executable)
	require.Error(t, err)
}

func TestWithDirectoryInPath(t *testing.T) {
	t.Parallel()
	separator := string(os.PathListSeparator)
	imagePath := strings.Join([]string{"/usr/local/bin", "/usr/bin", "/bin"}, separator)

	assert.Equal(t, "/opt/rc/bin"+separator+imagePath, WithDirectoryInPath(imagePath, "/opt/rc/bin"), "put the injected rc-kube first")
	assert.Equal(t, imagePath, WithDirectoryInPath(imagePath, "/usr/bin"), "keep a PATH that already has the directory")
	assert.Equal(t, "/opt/rc/bin", WithDirectoryInPath("", "/opt/rc/bin"))
}

func TestImageServeWrapper(t *testing.T) {
	if goruntime.GOOS == runtimeOSWindows {
		t.Skip("only Linux images wrap rc-kube serve")
	}
	injected := t.TempDir()
	executable := writeExecutable(t, filepath.Join(injected, "rc-kube"), "\x7fELF injected")
	script := t.TempDir()
	writeExecutable(t, filepath.Join(script, "rc-kube"), "#!/bin/sh\nexec /init\n")
	binary := t.TempDir()
	writeExecutable(t, filepath.Join(binary, "rc-kube"), "\x7fELF image")
	empty := t.TempDir()
	separator := string(os.PathListSeparator)

	assert.Equal(t, filepath.Join(script, "rc-kube"), ImageServeWrapper(strings.Join([]string{injected, empty, script, binary}, separator), executable), "keep the start sequence of an older image")
	assert.Empty(t, ImageServeWrapper(strings.Join([]string{injected, binary, script}, separator), executable), "an rc-kube binary in the image is not a start sequence")
	assert.Empty(t, ImageServeWrapper(strings.Join([]string{injected, empty}, separator), executable), "an image without rc-kube has no start sequence")
}
