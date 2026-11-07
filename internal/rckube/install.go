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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

const (
	// TiniName is the file name of the tini that rc-kube keeps next to itself.
	TiniName = "tini"
	// InitVariable names an image program that starts rc-kube serve when the
	// container starts it as PID 1. rc-kube runs it as
	// "$RC_KUBE_INIT RC-KUBE serve ARGUMENTS...", where RC-KUBE is the path of
	// the rc-kube to run. Without it, rc-kube starts itself under tini.
	InitVariable = "RC_KUBE_INIT"
	// installedMode lets every user run the copies but not change them.
	installedMode os.FileMode = 0o755
)

// Install copies the rc-kube executable and the tini next to it into
// directory. A Linux runtime Pod runs it in an init container, so that the
// runtime runs the rc-kube of the controller and not the one of its image.
func Install(executable, directory string) error {
	sources := []string{
		executable,
		filepath.Join(filepath.Dir(executable), TiniName),
	}
	for _, source := range sources {
		if err := installFile(source, filepath.Join(directory, filepath.Base(source))); err != nil {
			return err
		}
	}

	return nil
}

// installFile writes a temporary copy and renames it, so a container never
// sees a partial file, even when the init container runs again.
func installFile(source, target string) (err error) {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+"-*")
	if err != nil {
		return fmt.Errorf("create a copy of %s: %w", source, err)
	}
	defer func() {
		if err != nil {
			_ = output.Close()
			_ = os.Remove(output.Name())
		}
	}()
	if _, err = io.Copy(output, input); err != nil {
		return fmt.Errorf("copy %s: %w", source, err)
	}
	if err = output.Chmod(installedMode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", target, err)
	}
	if err = output.Close(); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err = os.Rename(output.Name(), target); err != nil {
		return fmt.Errorf("install %s: %w", target, err)
	}

	return nil
}

// FindTini returns the tini next to executable. An rc-kube that an image
// installed without tini next to it uses the tini on PATH.
func FindTini(executable string) (string, error) {
	sibling := filepath.Join(filepath.Dir(executable), TiniName)
	if info, err := os.Stat(sibling); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return sibling, nil
	}
	found, err := exec.LookPath(TiniName)
	if err != nil {
		return "", fmt.Errorf("rc-kube serve as PID 1 requires tini next to rc-kube or on PATH: %w", err)
	}

	return found, nil
}

// WithDirectoryInPath returns pathList with directory first, unless
// pathList already has it. Processes that rc-kube starts inherit this PATH,
// so a command such as "rc-kube process touch" reaches the same rc-kube.
func WithDirectoryInPath(pathList, directory string) string {
	entries := filepath.SplitList(pathList)
	if slices.Contains(entries, directory) {
		return pathList
	}
	if pathList == "" {
		return directory
	}

	return directory + string(os.PathListSeparator) + pathList
}

// ImageServeWrapper returns the rc-kube script that an image puts on PATH to
// start rc-kube serve under its own init system. Images built before rc-kube
// was injected did this through a script with the rc-kube name; such an image
// keeps its own start sequence. It returns an empty string when the first
// other rc-kube on PATH is not a script, or when there is none.
func ImageServeWrapper(pathList, executable string) string {
	self, selfErr := os.Stat(executable)
	for _, directory := range filepath.SplitList(pathList) {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, filepath.Base(executable))
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		if selfErr == nil && os.SameFile(self, info) {
			continue
		}
		if isScript(candidate) {
			return candidate
		}

		return ""
	}

	return ""
}

func isScript(name string) bool {
	file, err := os.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 2)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}

	return bytes.Equal(header, []byte("#!"))
}
