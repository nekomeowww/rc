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

// Package execd provides reusable OpenSandbox execd v1.0.0 protocol types,
// headers, mode conversions, glob matching, and SSE framing helpers.
package execd

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// Version is the OpenSandbox execd OpenAPI version implemented by rc.
	Version = "1.0.0"
	// DefaultPort is the standard OpenSandbox execd TCP port.
	DefaultPort = 44772
	// AccessTokenHeader is the standard OpenSandbox execd authentication header.
	AccessTokenHeader = "X-EXECD-ACCESS-TOKEN"
	// TailCursorHeader carries the next byte offset on GET /command/{id}/logs.
	TailCursorHeader = "EXECD-COMMANDS-TAIL-CURSOR"
	// VersionHeader advertises the execd server version.
	VersionHeader = "X-Execd-Version"
)

// RunCommandRequest is the JSON request body for POST /command.
type RunCommandRequest struct {
	Command    string            `json:"command,omitempty"`
	Argv       []string          `json:"argv,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	Background bool              `json:"background,omitempty"`
	Timeout    int64             `json:"timeout,omitempty"`
	UID        *int32            `json:"uid,omitempty"`
	GID        *int32            `json:"gid,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
}

// StreamError describes a command failure or timeout inside a ServerStreamEvent.
type StreamError struct {
	EName    string   `json:"ename"`
	EValue   string   `json:"evalue"`
	ExitCode *int32   `json:"exit_code,omitempty"`
	Trace    []string `json:"traceback,omitempty"`
}

// ServerStreamEvent is one SSE data payload emitted by POST /command.
type ServerStreamEvent struct {
	Type          string       `json:"type"`
	Text          string       `json:"text,omitempty"`
	ExitCode      *int32       `json:"exit_code,omitempty"`
	ExecutionTime *int64       `json:"execution_time,omitempty"`
	Timestamp     int64        `json:"timestamp"`
	Error         *StreamError `json:"error,omitempty"`
}

// CommandStatusResponse is the JSON response for GET /command/status/{id}.
type CommandStatusResponse struct {
	ID         string     `json:"id"`
	Content    string     `json:"content"`
	Running    bool       `json:"running"`
	ExitCode   *int32     `json:"exit_code"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// FileInfo is the OpenSandbox execd metadata representation of a file or directory.
type FileInfo struct {
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	CreatedAt  time.Time `json:"created_at"`
	Owner      string    `json:"owner"`
	Group      string    `json:"group"`
	Mode       int       `json:"mode"`
}

// Permission specifies mode/owner/group changes for POST /files/permissions and POST /directories.
type Permission struct {
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	Mode  int    `json:"mode"`
}

// FileMetadata is the JSON metadata part preceding each file part in POST /files/upload.
type FileMetadata struct {
	Path  string `json:"path"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	Mode  int    `json:"mode,omitempty"`
}

// RenameFileItem is one entry in POST /files/mv.
type RenameFileItem struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

// ReplaceFileContentItem is one file replacement spec in POST /files/replace.
type ReplaceFileContentItem struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ReplaceFileContentResult is returned per file when POST /files/replace?verbose=true.
type ReplaceFileContentResult struct {
	ReplacedCount int `json:"replacedCount"`
}

// ErrorResponse is the standard JSON error payload for execd HTTP endpoints.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes a JSON ErrorResponse with status code.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Code:    code,
		Message: message,
	})
}

// WriteSSE serializes and flushes one ServerStreamEvent as an SSE frame.
func WriteSSE(w http.ResponseWriter, flusher http.Flusher, ev ServerStreamEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// ToOctalInt formats a Unix permission bitmask as OpenSandbox's decimal-encoded
// octal integer (e.g., 0o755 -> 755, 0o644 -> 644).
func ToOctalInt(perm fs.FileMode) int {
	val, err := strconv.Atoi(fmt.Sprintf("%o", perm&0o777))
	if err != nil {
		return 644
	}
	return val
}

// ParseOctalMode parses OpenSandbox's decimal-encoded octal integer (e.g., 755 -> 0o755).
func ParseOctalMode(raw int, fallback fs.FileMode) (fs.FileMode, error) {
	if raw == 0 {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(strconv.Itoa(raw), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal mode %d", raw)
	}
	return fs.FileMode(parsed), nil
}

// MatchGlob checks whether relSlash or baseName matches an execd glob pattern.
func MatchGlob(pattern, relSlash, baseName string) bool {
	if pattern == "**" || pattern == "*" {
		return true
	}
	if strings.Contains(pattern, "/") {
		cleaned := strings.TrimPrefix(pattern, "**/")
		if ok, _ := filepath.Match(cleaned, relSlash); ok {
			return true
		}
	}
	ok, _ := filepath.Match(pattern, baseName)
	return ok
}
