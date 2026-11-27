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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/pkg/execd"
)

const (
	maxLogChunkBytes    = 16 << 20
	codeCommandNotFound = "COMMAND_NOT_FOUND"
)

func (s *Server) handleRunCommand(c echo.Context) error {
	w := c.Response().Writer
	r := c.Request()

	var req execd.RunCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", fmt.Sprintf("decode request: %v", err))
		return nil
	}
	if req.GID != nil && req.UID == nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", "gid requires uid")
		return nil
	}

	cmd, args, content, err := resolveCommand(req)
	if err != nil {
		execd.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST_BODY", err.Error())
		return nil
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
		return nil
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
		return nil
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
		return nil
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
			return nil
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
						return nil
					}
					if outcome.ExitCode == 0 {
						zero := int32(0)
						execd.WriteSSE(w, flusher, execd.ServerStreamEvent{
							Type:          "execution_complete",
							ExitCode:      &zero,
							ExecutionTime: &elapsed,
							Timestamp:     time.Now().UnixMilli(),
						})
						return nil
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
					return nil
				}
			}
		}
	}
}

func (s *Server) handleInterruptCommand(c echo.Context) error {
	id := strings.TrimSpace(c.QueryParam("id"))
	if id == "" {
		return c.JSON(http.StatusBadRequest, execd.ErrorResponse{
			Code:    "MISSING_QUERY_PARAMETER",
			Message: "id query parameter is required",
		})
	}
	if _, err := s.supervisor.Stop(id); err != nil {
		if errors.Is(err, processruntime.ErrNotFound) {
			return c.JSON(http.StatusNotFound, execd.ErrorResponse{
				Code:    codeCommandNotFound,
				Message: fmt.Sprintf("command %q not found", id),
			})
		}
		return c.JSON(http.StatusInternalServerError, execd.ErrorResponse{
			Code:    "COMMAND_STOP_FAILED",
			Message: err.Error(),
		})
	}
	return c.NoContent(http.StatusOK)
}

func (s *Server) handleCommandStatus(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return c.JSON(http.StatusBadRequest, execd.ErrorResponse{
			Code:    "INVALID_PATH_PARAMETER",
			Message: "command id is required",
		})
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
				return c.JSON(http.StatusNotFound, execd.ErrorResponse{
					Code:    "COMMAND_NOT_FOUND",
					Message: fmt.Sprintf("command %q not found", id),
				})
			}
			return c.JSON(http.StatusInternalServerError, execd.ErrorResponse{
				Code:    "COMMAND_STATUS_FAILED",
				Message: err.Error(),
			})
		}
		resp = execd.CommandStatusResponse{
			ID:       state.ID,
			Running:  state.Phase == "Running" || state.Phase == "Starting",
			ExitCode: state.ExitCode,
			Error:    state.Reason,
		}
	}

	return c.JSON(http.StatusOK, resp)
}

func (s *Server) handleCommandLogs(c echo.Context) error {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		return c.JSON(http.StatusBadRequest, execd.ErrorResponse{
			Code:    "INVALID_PATH_PARAMETER",
			Message: "command id is required",
		})
	}
	var cursor int64
	if rawCursor := strings.TrimSpace(c.QueryParam("cursor")); rawCursor != "" {
		parsed, err := strconv.ParseInt(rawCursor, 10, 64)
		if err != nil || parsed < 0 {
			return c.JSON(http.StatusBadRequest, execd.ErrorResponse{
				Code:    "INVALID_QUERY_PARAMETER",
				Message: "cursor must be a non-negative integer",
			})
		}
		cursor = parsed
	}

	var buf bytes.Buffer
	if err := s.supervisor.Logs(id, &buf); err != nil {
		if errors.Is(err, processruntime.ErrNotFound) {
			return c.JSON(http.StatusNotFound, execd.ErrorResponse{
				Code:    codeCommandNotFound,
				Message: fmt.Sprintf("command %q not found", id),
			})
		}
		return c.JSON(http.StatusInternalServerError, execd.ErrorResponse{
			Code:    "COMMAND_LOGS_FAILED",
			Message: err.Error(),
		})
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

	c.Response().Header().Set(execd.TailCursorHeader, strconv.FormatInt(nextCursor, 10))
	return c.Blob(http.StatusOK, "text/plain; charset=utf-8", slice)
}

func resolveCommand(req execd.RunCommandRequest) (string, []string, string, error) {
	hasCommand := strings.TrimSpace(req.Command) != ""
	hasArgv := len(req.Argv) > 0
	if hasCommand == hasArgv {
		return "", nil, "", errors.New("supply exactly one of command or argv")
	}
	if hasArgv {
		if req.Argv[0] == "" {
			return "", nil, "", errors.New("argv[0] must be non-empty")
		}
		return req.Argv[0], req.Argv[1:], strings.Join(req.Argv, " "), nil
	}
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/C", req.Command}, req.Command, nil
	}
	return "/bin/sh", []string{"-c", req.Command}, req.Command, nil
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
