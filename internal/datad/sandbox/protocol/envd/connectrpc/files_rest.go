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

package envdconnectrpc

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nekomeowww/rc/pkg/envd"
)

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	if err := s.validateUser(r.URL.Query().Get("username")); err != nil {
		envd.WriteJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}
	rawPath := r.URL.Query().Get("path")
	if strings.TrimSpace(rawPath) == "" {
		envd.WriteJSONError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}

	useGzip, err := envd.NegotiateEncoding(r.Header.Get("Accept-Encoding"))
	if err != nil {
		envd.WriteJSONError(w, http.StatusNotAcceptable, err.Error())
		return
	}

	resolved := s.resolvePath(rawPath)
	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			envd.WriteJSONError(w, http.StatusNotFound, fmt.Sprintf("path %q does not exist", resolved))
			return
		}
		envd.WriteJSONError(w, http.StatusInternalServerError, fmt.Sprintf("stat %q: %v", resolved, err))
		return
	}
	if info.IsDir() {
		envd.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("path %q is a directory", resolved))
		return
	}

	file, err := os.Open(resolved)
	if err != nil {
		envd.WriteJSONError(w, http.StatusInternalServerError, fmt.Sprintf("open %q: %v", resolved, err))
		return
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	if useGzip {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = io.Copy(gz, file)
		_ = gz.Close()
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (s *Server) handleUploadFiles(w http.ResponseWriter, r *http.Request) {
	if err := s.validateUser(r.URL.Query().Get("username")); err != nil {
		envd.WriteJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}
	meta, err := envd.ParseMetadataHeaders(r.Header)
	if err != nil {
		envd.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	contentType := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		s.handleMultipartUpload(w, r, meta)
	default:
		s.handleOctetStreamUpload(w, r, meta)
	}
}

func (s *Server) handleOctetStreamUpload(w http.ResponseWriter, r *http.Request, meta map[string]string) {
	rawPath := r.URL.Query().Get("path")
	if strings.TrimSpace(rawPath) == "" {
		envd.WriteJSONError(w, http.StatusBadRequest, "path query parameter is required for octet-stream upload")
		return
	}
	resolved := s.resolvePath(rawPath)

	var body io.Reader = r.Body
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			envd.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid gzip body: %v", err))
			return
		}
		defer func() { _ = gz.Close() }()
		body = gz
	}

	entry, status, err := s.writeUploadedFile(resolved, body, meta)
	if err != nil {
		envd.WriteJSONError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode([]envd.RESTEntryInfo{entry})
}

func (s *Server) handleMultipartUpload(w http.ResponseWriter, r *http.Request, meta map[string]string) {
	reader, err := r.MultipartReader()
	if err != nil {
		envd.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid multipart body: %v", err))
		return
	}
	queryPath := strings.TrimSpace(r.URL.Query().Get("path"))
	entries := make([]envd.RESTEntryInfo, 0, 1)

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			envd.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("read multipart part: %v", err))
			return
		}
		targetRaw := multipartPartFilename(part)
		if queryPath != "" && (targetRaw == "" || len(entries) == 0 && !strings.Contains(targetRaw, "/")) {
			targetRaw = queryPath
		}
		if strings.TrimSpace(targetRaw) == "" {
			_ = part.Close()
			envd.WriteJSONError(w, http.StatusBadRequest, "file part must include a filename or path query parameter")
			return
		}
		resolved := s.resolvePath(targetRaw)
		entry, status, writeErr := s.writeUploadedFile(resolved, part, meta)
		_ = part.Close()
		if writeErr != nil {
			envd.WriteJSONError(w, status, writeErr.Error())
			return
		}
		entries = append(entries, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(entries)
}

func (s *Server) writeUploadedFile(resolved string, body io.Reader, meta map[string]string) (envd.RESTEntryInfo, int, error) {
	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		return envd.RESTEntryInfo{}, http.StatusBadRequest, fmt.Errorf("path %q is a directory", resolved)
	}
	parent := filepath.Dir(resolved)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return envd.RESTEntryInfo{}, httpStatusForFSError(err), fmt.Errorf("create parent directory %q: %w", parent, err)
	}
	file, err := os.OpenFile(resolved, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return envd.RESTEntryInfo{}, httpStatusForFSError(err), fmt.Errorf("open %q for write: %w", resolved, err)
	}
	if _, err := io.Copy(file, body); err != nil {
		_ = file.Close()
		return envd.RESTEntryInfo{}, httpStatusForFSError(err), fmt.Errorf("write %q: %w", resolved, err)
	}
	if err := file.Close(); err != nil {
		return envd.RESTEntryInfo{}, httpStatusForFSError(err), fmt.Errorf("close %q: %w", resolved, err)
	}
	s.setFileMetadata(resolved, meta)

	return envd.RESTEntryInfo{
		Path:     resolved,
		Name:     filepath.Base(resolved),
		Type:     "file",
		Metadata: meta,
	}, http.StatusOK, nil
}

func httpStatusForFSError(err error) int {
	if errors.Is(err, syscall.ENOSPC) {
		return http.StatusInsufficientStorage
	}
	return http.StatusInternalServerError
}

// multipartPartFilename reads the raw filename parameter from Content-Disposition
// without Go's stdlib filepath.Base stripping, since E2B SDK sends full target
// paths in the multipart filename field.
func multipartPartFilename(part *multipart.Part) string {
	if cd := part.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				return name
			}
		}
	}
	return part.FileName()
}
