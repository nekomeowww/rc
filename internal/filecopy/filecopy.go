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

// Package filecopy moves one file or directory tree as a tar stream. rcctl and
// rc-kube both use it, so a copy needs no tar binary in the runner image.
package filecopy

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Pack writes source, a file or a directory tree, to output. Entry names are
// rooted at the base name of source. Symbolic links are archived as links,
// never followed. Owners are not recorded.
func Pack(output io.Writer, source string) error {
	source = filepath.Clean(source)
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	base := filepath.Base(source)
	if base == "." || base == string(filepath.Separator) {
		return fmt.Errorf("copy source %q has no name; name a file or directory inside it", source)
	}
	writer := tar.NewWriter(output)
	if !info.IsDir() {
		if err := packEntry(writer, source, base, info); err != nil {
			return err
		}
		return writer.Close()
	}
	err = filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return packEntry(writer, name, path.Join(base, filepath.ToSlash(relative)), info)
	})
	if err != nil {
		return err
	}

	return writer.Close()
}

func packEntry(writer *tar.Writer, name, archiveName string, info fs.FileInfo) error {
	link := ""
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(name)
		if err != nil {
			return err
		}
		link = target
	case info.Mode().IsRegular(), info.IsDir():
	default:
		return fmt.Errorf("cannot copy %s: only files, directories, and symbolic links are supported", name)
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = archiveName
	if info.IsDir() {
		header.Name += "/"
	}
	header.Uid, header.Gid, header.Uname, header.Gname = 0, 0, "", ""
	header.Format = tar.FormatPAX
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(writer, file)

	return errors.Join(copyErr, file.Close())
}

// Unpack reads a stream from Pack and places its root at destination: inside
// destination when that is an existing directory, otherwise at destination
// itself, which must have an existing parent. Every entry is created through an
// os.Root, so no entry or link can write outside the chosen parent directory.
func Unpack(input io.Reader, destination string) error {
	destination = filepath.Clean(destination)
	reader := tar.NewReader(input)
	header, err := reader.Next()
	if errors.Is(err, io.EOF) {
		return errors.New("copy stream is empty")
	}
	if err != nil {
		return err
	}
	top, _, _ := strings.Cut(strings.TrimSuffix(header.Name, "/"), "/")
	parent, rootName := filepath.Dir(destination), filepath.Base(destination)
	if info, err := os.Stat(destination); err == nil && info.IsDir() {
		parent, rootName = destination, top
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("open copy destination: %w", err)
	}
	defer func() { _ = root.Close() }()
	for {
		if err := unpackEntry(root, reader, header, top, rootName); err != nil {
			return err
		}
		header, err = reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func unpackEntry(root *os.Root, reader *tar.Reader, header *tar.Header, top, rootName string) error {
	name := strings.TrimSuffix(header.Name, "/")
	rest, ok := strings.CutPrefix(name, top)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return fmt.Errorf("copy stream entry %q is outside its root %q", header.Name, top)
	}
	local, err := filepath.Localize(rootName + rest)
	if err != nil {
		return fmt.Errorf("copy stream entry %q: %w", header.Name, err)
	}
	mode := fs.FileMode(header.Mode).Perm()
	switch header.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(local, mode|0o700); err != nil {
			return err
		}
		return root.Chmod(local, mode|0o700)
	case tar.TypeReg:
		if err := root.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		file, err := root.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, reader)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return err
		}
		if err := root.Chmod(local, mode); err != nil {
			return err
		}
		return root.Chtimes(local, header.ModTime, header.ModTime)
	case tar.TypeSymlink:
		if err := root.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		if err := root.Remove(local); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return root.Symlink(header.Linkname, local)
	default:
		return fmt.Errorf("copy stream entry %q has unsupported type %q", header.Name, header.Typeflag)
	}
}
