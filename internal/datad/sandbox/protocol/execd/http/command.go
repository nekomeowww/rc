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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/pkg/execd"
)

const maxLogChunkBytes = 16 << 20

func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	var req execd.RunCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", fmt.Sprintf("decode request: %v", err))
		return
	}
	hasCommand := strings.TrimSpace(req.Command) != ""
	hasArgv := len(req.Argv) > 0
	if hasCommand == hasArgv {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "supply exactly one of command or argv")
		return
	}
	if req.GID != nil && req.UID == nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "gid requires uid")
		return
	}

	var cmd string
	var args []string
	var content string
	if hasArgv {
		if req.Argv[0] == "" {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "argv[0] must be non-empty")
			return
		}
		cmd = req.Argv[0]
		args = req.Argv[1:]
		content = strings.Join(req.Argv, " ")
	} else {
		if runtime.GOOS == "windows" {
			cmd = "cmd.exe"
			args = []string{"/C", req.Command}
		} else {
			cmd = "/bin/sh"
			args = []string{"-c", req.Command}
		}
		content = req.Command
	}

	cwd := s.workspaceDir
	if strings.TrimSpace(req.Cwd) != "" {
		cwd = s.resolvePath(req.Cwd)
	}

	startedAt := time.Now().UTC()
	info, sub, err := s.supervisor.StartDataPlaneProcess(r.Context(), rckube.DataPlaneProcessConfig{
		Cmd:   cmd,
		Args:  args,
		Envs:  req.Envs,
		Cwd:   cwd,
		Stdin: false,
	})
	if err != nil {
		execd.WriteError(w, http.StatusInternalServerError, "COMMAND_START_FAILED", err.Error())
		return
	}

	rec := &commandRecord{
		id:        info.ID,
		content:   content,
		startedAt: startedAt,
		running:   true,
	}
	s.cmdMu.Lock()
	s.commands[info.ID] = rec
	s.cmdMu.Unlock()

	var timeoutTimer *time.Timer
	timedOut := make(chan struct{}, 1)
	if req.Timeout > 0 {
		timeoutTimer = time.AfterFunc(time.Duration(req.Timeout)*time.Millisecond, func() {
			select {
			case timedOut <- struct{}{}:
			default:
			}
			_, _ = s.supervisor.Stop(info.ID)
		})
	}

	go func() {
		<-sub.Exit()
		if timeoutTimer != nil {
			timeoutTimer.Stop()
		}
		outcome := sub.Outcome()
		finished := time.Now().UTC()
		code := outcome.ExitCode
		s.cmdMu.Lock()
		rec.running = false
		rec.finishedAt = &finished
		rec.exitCode = &code
		rec.errMessage = outcome.Error
		s.cmdMu.Unlock()
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		sub.Close()
		execd.WriteError(w, http.StatusInternalServerError, "STREAMING_UNSUPPORTED", "response writer does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
		Type:      "init",
		Text:      info.ID,
		Timestamp: time.Now().UnixMilli(),
	})

	if req.Background {
		sub.Close()
		return
	}
	defer sub.Close()

	for _, chunk := range sub.Replay() {
		writeChunkSSE(w, flusher, chunk)
	}

	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
				Type:      "ping",
				Timestamp: time.Now().UnixMilli(),
			})
		case chunk := <-sub.Chunks():
			writeChunkSSE(w, flusher, chunk)
		case <-sub.Exit():
			for {
				select {
				case chunk := <-sub.Chunks():
					writeChunkSSE(w, flusher, chunk)
				default:
					outcome := sub.Outcome()
					elapsed := time.Since(startedAt).Milliseconds()
					isTimeout := false
					select {
					case <-timedOut:
						isTimeout = true
					default:
					}
					if isTimeout {
						code := outcome.ExitCode
						execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
							Type:          "error",
							ExecutionTime: &elapsed,
							Timestamp:     time.Now().UnixMilli(),
							Error: &execd.StreamError{
								EName:    "CommandTimeout",
								EValue:   fmt.Sprintf("command exceeded timeout of %dms", req.Timeout),
								ExitCode: &code,
							},
						})
						return
					}
					if outcome.ExitCode == 0 {
						zero := int32(0)
						execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
							Type:          "execution_complete",
							ExitCode:      &zero,
							ExecutionTime: &elapsed,
							Timestamp:     time.Now().UnixMilli(),
						})
						return
					}
					code := outcome.ExitCode
					evalue := strconv.Itoa(int(code))
					if outcome.Error != "" {
						evalue = outcome.Error
					}
					execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
						Type:          "error",
						ExecutionTime: &elapsed,
						Timestamp:     time.Now().UnixMilli(),
						Error: &execd.StreamError{
							EName:    "CommandExecError",
							EValue:   evalue,
							ExitCode: &code,
						},
					})
					return
				}
			}
		}
	}
}

func (s *Server) handleInterruptCommand(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		execd.WriteError(w, http.StatusBadRequest, "MISSING_QUERY_PARAMETER", "id query parameter is required")
		return
	}
	if _, err := s.supervisor.Stop(id); err != nil {
		if errors.Is(err, processruntime.ErrNotFound) {
			execd.WriteError(w, http.StatusNotFound, "COMMAND_NOT_FOUND", fmt.Sprintf("command %q not found", id))
			return
		}
		execd.WriteError(w, http.StatusInternalServerError, "COMMAND_STOP_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCommandSubroute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/command/")
	if id, ok := strings.CutPrefix(rest, "status/"); ok && id != "" && !strings.Contains(id, "/") {
		s.handleCommandStatus(w, r, id)
		return
	}
	if id, ok := strings.CutSuffix(rest, "/logs"); ok && id != "" && !strings.Contains(id, "/") {
		s.handleCommandLogs(w, r, id)
		return
	}
	execd.WriteError(w, http.StatusNotFound, "NOT_FOUND", "unknown command endpoint")
}

func (s *Server) handleCommandStatus(w http.ResponseWriter, _ *http.Request, id string) {
	if id == "" {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_PATH_PARAMETER", "command id is required")
		return
	}

	s.cmdMu.RLock()
	rec := s.commands[id]
	var resp execd.CommandStatusResponse
	if rec != nil {
		resp = execd.CommandStatusResponse{
			ID:         rec.id,
			Content:    rec.content,
			Running:    rec.running,
			ExitCode:   rec.exitCode,
			Error:      rec.errMessage,
			StartedAt:  rec.startedAt,
			FinishedAt: rec.finishedAt,
		}
	}
	s.cmdMu.RUnlock()

	if rec == nil {
		state, err := s.supervisor.Inspect(id)
		if err != nil {
			if errors.Is(err, processruntime.ErrNotFound) {
				execd.WriteError(w, http.StatusNotFound, "COMMAND_NOT_FOUND", fmt.Sprintf("command %q not found", id))
				return
			}
			execd.WriteError(w, http.StatusInternalServerError, "COMMAND_STATUS_FAILED", err.Error())
			return
		}
		resp = execd.CommandStatusResponse{
			ID:       state.ID,
			Running:  state.Phase == "Running" || state.Phase == "Starting",
			ExitCode: state.ExitCode,
			Error:    state.Reason,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCommandLogs(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_PATH_PARAMETER", "command id is required")
		return
	}
	var cursor int64
	if rawCursor := strings.TrimSpace(r.URL.Query().Get("cursor")); rawCursor != "" {
		parsed, err := strconv.ParseInt(rawCursor, 10, 64)
		if err != nil || parsed < 0 {
			execd.WriteError(w, http.StatusBadRequest, "INVALID_QUERY_PARAMETER", "cursor must be a non-negative integer")
			return
		}
		cursor = parsed
	}

	var buf bytes.Buffer
	if err := s.supervisor.Logs(id, &buf); err != nil {
		if errors.Is(err, processruntime.ErrNotFound) {
			execd.WriteError(w, http.StatusNotFound, "COMMAND_NOT_FOUND", fmt.Sprintf("command %q not found", id))
			return
		}
		execd.WriteError(w, http.StatusInternalServerError, "COMMAND_LOGS_FAILED", err.Error())
		return
	}

	all := buf.Bytes()
	var slice []byte
	if cursor < int64(len(all)) {
		slice = all[cursor:]
		if len(slice) > maxLogChunkBytes {
			slice = slice[:maxLogChunkBytes]
		}
	}
	nextCursor := cursor + int64(len(slice))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(execd.TailCursorHeader, strconv.FormatInt(nextCursor, 10))
	w.WriteHeader(http.StatusOK)
	if len(slice) > 0 {
		_, _ = w.Write(slice)
	}
}

func writeChunkSSE(w http.ResponseWriter, flusher http.Flusher, chunk rckube.DataPlaneChunk) {
	if len(chunk.Data) == 0 {
		return
	}
	evType := "stdout"
	if chunk.Kind == rckube.DataPlaneStreamStderr {
		evType = "stderr"
	}
	execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
		Type:      evType,
		Text:      string(chunk.Data),
		Timestamp: time.Now().UnixMilli(),
	})
}

var _ context.Context
