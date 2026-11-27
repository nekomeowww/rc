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

package execdws_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	execdws "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/execd/ws"
	"github.com/nekomeowww/rc/internal/rckube"
)

func TestWebSocketPTYInteractiveAndResize(t *testing.T) {
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))

	supervisor := rckube.NewSupervisor(filepath.Join(tmp, "state"), 2*time.Second, rckube.WithRoots(homeDir, homeDir, filepath.Join(tmp, "run")))
	t.Cleanup(supervisor.Shutdown)

	ts := httptest.NewServer(execdws.NewHandler(supervisor, homeDir))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "?cols=80&rows=24"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Resize frame.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":120,"rows":40}`)))
	// Execute command and exit.
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, []byte("echo WS_PTY_OK && exit\n")))

	var out strings.Builder
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, msg, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}
		out.Write(msg)
		if strings.Contains(out.String(), "WS_PTY_OK") {
			break
		}
	}
	assert.Contains(t, out.String(), "WS_PTY_OK")
}
