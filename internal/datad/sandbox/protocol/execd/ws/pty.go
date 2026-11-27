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

// Package execdws implements the OpenSandbox execd WebSocket PTY protocol
// backed by rckube.Supervisor.
package execdws

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/nekomeowww/rc/internal/rckube"
)

type controlMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// Handler upgrades GET /pty requests to an interactive PTY WebSocket session.
type Handler struct {
	supervisor   *rckube.Supervisor
	workspaceDir string
	upgrader     websocket.Upgrader
}

// NewHandler creates a WebSocket PTY handler backed by supervisor.
func NewHandler(supervisor *rckube.Supervisor, workspaceDir string) *Handler {
	return &Handler{
		supervisor:   supervisor,
		workspaceDir: workspaceDir,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
	}
}

// ServeHTTP upgrades the connection, starts a PTY process under supervisor,
// and bridges WebSocket frames to/from the PTY until either side closes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cols := parseUint16(q.Get("cols"), 80)
	rows := parseUint16(q.Get("rows"), 24)
	cwd := strings.TrimSpace(q.Get("cwd"))
	if cwd == "" {
		cwd = h.workspaceDir
	}

	cmd := strings.TrimSpace(q.Get("cmd"))
	var args []string
	if cmd == "" {
		if runtime.GOOS == "windows" {
			cmd = "cmd.exe"
		} else {
			cmd = "/bin/sh"
		}
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	info, sub, err := h.supervisor.StartDataPlaneProcess(r.Context(), rckube.DataPlaneProcessConfig{
		Cmd:   cmd,
		Args:  args,
		Cwd:   cwd,
		PTY:   true,
		Cols:  cols,
		Rows:  rows,
		Stdin: true,
	})
	if err != nil {
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
		return
	}
	defer sub.Close()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			msgType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				_, _ = h.supervisor.Stop(info.ID)
				return
			}
			if msgType == websocket.TextMessage && len(payload) > 0 && payload[0] == '{' {
				var ctrl controlMessage
				if json.Unmarshal(payload, &ctrl) == nil && ctrl.Type == "resize" && ctrl.Cols > 0 && ctrl.Rows > 0 {
					_ = h.supervisor.ResizeDataPlanePTY(info.PID, "", ctrl.Cols, ctrl.Rows)
					continue
				}
			}
			if len(payload) > 0 {
				_ = h.supervisor.SendDataPlaneInput(info.PID, "", payload)
			}
		}
	}()

	for _, chunk := range sub.Replay() {
		if err := conn.WriteMessage(websocket.BinaryMessage, chunk.Data); err != nil {
			return
		}
	}

	for {
		select {
		case <-readDone:
			return
		case chunk := <-sub.Chunks():
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk.Data); err != nil {
				return
			}
		case <-sub.Exit():
			for {
				select {
				case chunk := <-sub.Chunks():
					_ = conn.WriteMessage(websocket.BinaryMessage, chunk.Data)
				default:
					_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "process exited"))
					return
				}
			}
		}
	}
}

func parseUint16(raw string, fallback uint16) uint16 {
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || v == 0 {
		return fallback
	}
	return uint16(v)
}
