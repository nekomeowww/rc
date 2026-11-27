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

package execdhttp

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/nekomeowww/rc/pkg/execd"
)

func (s *Server) handleFilesInfo(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "at least one path query parameter is required")
		return nil
	}
	result := make(map[string]execd.FileInfo, len(paths))
	for _, raw := range paths {
		resolved := s.resolvePath(raw)
		info, err := s.statFileInfo(resolved)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				execd.WriteError(w, http.StatusNotFound, "FILE_NOT_FOUND", fmt.Sprintf("path %q does not exist", resolved))
				return nil
			}
			execd.WriteError(w, http.StatusInternalServerError, "STAT_FAILED", err.Error())
			return nil
		}
		result[raw] = info
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) handleRemoveFiles(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "at least one path query parameter is required")
		return nil
	}
	for _, raw := range paths {
		resolved := s.resolvePath(raw)
		info, err := os.Lstat(resolved)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			execd.WriteError(w, http.StatusInternalServerError, "REMOVE_FAILED", err.Error())
			return nil
		}
		if info.IsDir() {
			execd.WriteError(w, http.StatusBadRequest, "IS_DIRECTORY", fmt.Sprintf("path %q is a directory; use DELETE /directories", resolved))
			return nil
		}
		if err := os.Remove(resolved); err != nil {
			execd.WriteError(w, http.StatusInternalServerError, "REMOVE_FAILED", err.Error())
			return nil
		}
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleChmodFiles(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	var body map[string]execd.Permission
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
		return nil
	}
	for raw, perm := range body {
		resolved := s.resolvePath(raw)
		mode, err := execd.ParseOctalMode(perm.Mode, 0o644)
		if err != nil {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_MODE", err.Error())
			return nil
		}
		if err := os.Chmod(resolved, mode); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				execd.WriteError(w, http.StatusNotFound, "FILE_NOT_FOUND", err.Error())
				return nil
			}
			execd.WriteError(w, http.StatusInternalServerError, "CHMOD_FAILED", err.Error())
			return nil
		}
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleRenameFiles(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	var items []execd.RenameFileItem
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
		return nil
	}
	for _, item := range items {
		if strings.TrimSpace(item.Src) == "" || strings.TrimSpace(item.Dest) == "" {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "src and dest must be non-empty")
			return nil
		}
	}
	for _, item := range items {
		src := s.resolvePath(item.Src)
		dst := s.resolvePath(item.Dest)
		if err := os.Rename(src, dst); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				execd.WriteError(w, http.StatusNotFound, "FILE_NOT_FOUND", err.Error())
				return nil
			}
			execd.WriteError(w, http.StatusInternalServerError, "RENAME_FAILED", err.Error())
			return nil
		}
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleSearchFiles(c echo.Context) error {
	w := c.Response().Writer
	rawPath := strings.TrimSpace(c.QueryParam("path"))
	if rawPath == "" {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "path query parameter is required")
		return nil
	}
	pattern := strings.TrimSpace(c.QueryParam("pattern"))
	if pattern == "" {
		pattern = "**"
	}
	root := s.resolvePath(rawPath)
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			execd.WriteError(w, http.StatusNotFound, "FILE_NOT_FOUND", err.Error())
			return nil
		}
		execd.WriteError(w, http.StatusInternalServerError, "SEARCH_FAILED", err.Error())
		return nil
	}
	if !info.IsDir() {
		execd.WriteError(w, http.StatusBadRequest, "NOT_A_DIRECTORY", fmt.Sprintf("path %q is not a directory", root))
		return nil
	}

	matches := make([]execd.FileInfo, 0)
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		if !execd.MatchGlob(pattern, filepath.ToSlash(rel), d.Name()) {
			return nil
		}
		fi, statErr := s.statFileInfo(p)
		if statErr == nil {
			matches = append(matches, fi)
		}
		return nil
	})
	if walkErr != nil {
		execd.WriteError(w, http.StatusInternalServerError, "SEARCH_FAILED", walkErr.Error())
		return nil
	}

	return c.JSON(http.StatusOK, matches)
}

func (s *Server) handleReplaceContent(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	var body map[string]execd.ReplaceFileContentItem
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
		return nil
	}
	verbose := strings.EqualFold(c.QueryParam("verbose"), "true")
	results := make(map[string]execd.ReplaceFileContentResult, len(body))

	for raw, item := range body {
		if item.Old == "" {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "old string must not be empty")
			return nil
		}
		resolved := s.resolvePath(raw)
		stat, err := os.Stat(resolved)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				execd.WriteError(w, http.StatusNotFound, "FILE_NOT_FOUND", err.Error())
				return nil
			}
			execd.WriteError(w, http.StatusInternalServerError, "REPLACE_FAILED", err.Error())
			return nil
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			execd.WriteError(w, http.StatusInternalServerError, "REPLACE_FAILED", err.Error())
			return nil
		}
		original := string(data)
		count := strings.Count(original, item.Old)
		if count > 0 {
			updated := strings.ReplaceAll(original, item.Old, item.New)
			if err := os.WriteFile(resolved, []byte(updated), stat.Mode().Perm()); err != nil {
				execd.WriteError(w, http.StatusInternalServerError, "REPLACE_FAILED", err.Error())
				return nil
			}
		}
		results[raw] = execd.ReplaceFileContentResult{ReplacedCount: count}
	}

	if verbose {
		return c.JSON(http.StatusOK, results)
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleUploadFiles(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	reader, err := r.MultipartReader()
	if err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", fmt.Sprintf("expected multipart/form-data: %v", err))
		return nil
	}
	var pendingMeta *execd.FileMetadata
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
			return nil
		}
		switch part.FormName() {
		case "metadata":
			var meta execd.FileMetadata
			if decErr := json.NewDecoder(part).Decode(&meta); decErr != nil {
				_ = part.Close()
				execd.WriteError(w, http.StatusBadRequest, "INVALID_METADATA", decErr.Error())
				return nil
			}
			_ = part.Close()
			pendingMeta = &meta
		case "file":
			targetRaw := ""
			modeVal := 0
			if pendingMeta != nil {
				targetRaw = pendingMeta.Path
				modeVal = pendingMeta.Mode
				pendingMeta = nil
			}
			if targetRaw == "" {
				targetRaw = part.FileName()
			}
			if strings.TrimSpace(targetRaw) == "" {
				_ = part.Close()
				execd.WriteError(w, http.StatusBadRequest, "MISSING_PATH", "upload requires metadata.path or filename")
				return nil
			}
			resolved := s.resolvePath(targetRaw)
			if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
				_ = part.Close()
				execd.WriteError(w, http.StatusInternalServerError, "UPLOAD_FAILED", err.Error())
				return nil
			}
			mode, err := execd.ParseOctalMode(modeVal, 0o644)
			if err != nil {
				_ = part.Close()
				execd.WriteError(w, http.StatusBadRequest, "INVALID_MODE", err.Error())
				return nil
			}
			out, err := os.OpenFile(resolved, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				_ = part.Close()
				execd.WriteError(w, http.StatusInternalServerError, "UPLOAD_FAILED", err.Error())
				return nil
			}
			if _, copyErr := io.Copy(out, part); copyErr != nil {
				_ = out.Close()
				_ = part.Close()
				execd.WriteError(w, http.StatusInternalServerError, "UPLOAD_FAILED", copyErr.Error())
				return nil
			}
			_ = out.Close()
			_ = part.Close()
		default:
			_ = part.Close()
		}
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleDownloadFile(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	rawPath := strings.TrimSpace(c.QueryParam("path"))
	if rawPath == "" {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "path query parameter is required")
		return nil
	}
	rawOffset := strings.TrimSpace(c.QueryParam("offset"))
	rawLimit := strings.TrimSpace(c.QueryParam("limit"))
	hasLineParams := rawOffset != "" || rawLimit != ""
	if hasLineParams && r.Header.Get("Range") != "" {
		execd.WriteError(w, http.StatusBadRequest, "CONFLICTING_PARAMETERS", "offset/limit are mutually exclusive with Range header")
		return nil
	}

	resolved := s.resolvePath(rawPath)
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			execd.WriteError(w, http.StatusNotFound, "FILE_NOT_FOUND", err.Error())
			return nil
		}
		execd.WriteError(w, http.StatusInternalServerError, "DOWNLOAD_FAILED", err.Error())
		return nil
	}
	if info.IsDir() {
		execd.WriteError(w, http.StatusBadRequest, "IS_DIRECTORY", fmt.Sprintf("path %q is a directory", resolved))
		return nil
	}

	file, err := os.Open(resolved)
	if err != nil {
		execd.WriteError(w, http.StatusInternalServerError, "DOWNLOAD_FAILED", err.Error())
		return nil
	}
	defer func() { _ = file.Close() }()

	if hasLineParams {
		offset := 1
		if rawOffset != "" {
			parsed, parseErr := strconv.Atoi(rawOffset)
			if parseErr != nil || parsed < 1 {
				execd.WriteError(w, http.StatusBadRequest, "INVALID_QUERY_PARAMETER", "offset must be >= 1")
				return nil
			}
			offset = parsed
		}
		limit := -1
		if rawLimit != "" {
			parsed, parseErr := strconv.Atoi(rawLimit)
			if parseErr != nil || parsed < 1 {
				execd.WriteError(w, http.StatusBadRequest, "INVALID_QUERY_PARAMETER", "limit must be >= 1")
				return nil
			}
			limit = parsed
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		reader := bufio.NewReader(file)
		lineNo := 0
		emitted := 0
		for {
			line, readErr := reader.ReadString('\n')
			if len(line) > 0 {
				lineNo++
				if lineNo >= offset {
					if limit > 0 && emitted >= limit {
						break
					}
					_, _ = io.WriteString(w, line)
					emitted++
				}
			}
			if readErr != nil {
				break
			}
		}
		return nil
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(resolved)))
	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), file)
	return nil
}

func (s *Server) handleListDirectory(c echo.Context) error {
	w := c.Response().Writer
	rawPath := strings.TrimSpace(c.QueryParam("path"))
	if rawPath == "" {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "path query parameter is required")
		return nil
	}
	depth := 1
	if rawDepth := strings.TrimSpace(c.QueryParam("depth")); rawDepth != "" {
		parsed, err := strconv.Atoi(rawDepth)
		if err != nil || parsed < 0 {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_QUERY_PARAMETER", "depth must be >= 0")
			return nil
		}
		depth = parsed
	}
	resolved := s.resolvePath(rawPath)
	linfo, err := os.Lstat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			execd.WriteError(w, http.StatusNotFound, "DIRECTORY_NOT_FOUND", err.Error())
			return nil
		}
		execd.WriteError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return nil
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		execd.WriteError(w, http.StatusBadRequest, "SYMLINK_NOT_ALLOWED", "root path cannot be a symbolic link")
		return nil
	}
	if !linfo.IsDir() {
		execd.WriteError(w, http.StatusBadRequest, "NOT_A_DIRECTORY", fmt.Sprintf("path %q is not a directory", resolved))
		return nil
	}

	entries := make([]execd.FileInfo, 0)
	if depth > 0 {
		if err := s.listRecursive(resolved, 1, depth, &entries); err != nil {
			execd.WriteError(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
			return nil
		}
	}
	return c.JSON(http.StatusOK, entries)
}

func (s *Server) listRecursive(dir string, currentDepth, maxDepth int, out *[]execd.FileInfo) error {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	slices.SortFunc(dirEntries, func(a, b os.DirEntry) int {
		return cmp.Compare(a.Name(), b.Name())
	})
	for _, entry := range dirEntries {
		full := filepath.Join(dir, entry.Name())
		fi, err := s.statFileInfo(full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		*out = append(*out, fi)
		if fi.Type == "directory" && currentDepth < maxDepth {
			if err := s.listRecursive(full, currentDepth+1, maxDepth, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) handleMakeDirs(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	var body map[string]execd.Permission
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
		return nil
	}
	for raw, perm := range body {
		resolved := s.resolvePath(raw)
		mode, err := execd.ParseOctalMode(perm.Mode, 0o755)
		if err != nil {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_MODE", err.Error())
			return nil
		}
		if err := os.MkdirAll(resolved, mode); err != nil {
			execd.WriteError(w, http.StatusInternalServerError, "MKDIR_FAILED", err.Error())
			return nil
		}
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleRemoveDirs(c echo.Context) error {
	w, r := c.Response().Writer, c.Request()
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "at least one path query parameter is required")
		return nil
	}
	for _, raw := range paths {
		resolved := s.resolvePath(raw)
		if err := os.RemoveAll(resolved); err != nil {
			execd.WriteError(w, http.StatusInternalServerError, "RMDIR_FAILED", err.Error())
			return nil
		}
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) statFileInfo(path string) (execd.FileInfo, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		return execd.FileInfo{}, err
	}
	entryType := "other"
	switch {
	case linfo.Mode()&os.ModeSymlink != 0:
		entryType = "symlink"
	case linfo.IsDir():
		entryType = "directory"
	case linfo.Mode().IsRegular():
		entryType = "file"
	}
	modTime := linfo.ModTime().UTC()
	return execd.FileInfo{
		Path:       path,
		Type:       entryType,
		Size:       linfo.Size(),
		ModifiedAt: modTime,
		CreatedAt:  modTime,
		Owner:      s.runtimeUser,
		Group:      s.runtimeUser,
		Mode:       execd.ToOctalInt(linfo.Mode().Perm()),
	}, nil
}
