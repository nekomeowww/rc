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

package execdhttp_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nekomeowww/rc/internal/datad/pki"
	execdhttp "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/execd/http"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/pkg/execd"
)

func startTestExecdServer(t *testing.T) (baseURL string, client *http.Client, supervisor *rckube.Supervisor, workspaceDir string) {
	t.Helper()
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	workspaceDir = filepath.Join(tmp, "workspace")
	stateDir := filepath.Join(tmp, "state")
	runDir := filepath.Join(tmp, "run")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	ca, err := pki.GenerateCA(time.Hour)
	require.NoError(t, err)
	serverName := pki.WorkspaceServerName("default", "demo")
	srvCert, err := pki.IssueServerCert(ca.CertPEM, ca.KeyPEM, serverName, time.Hour)
	require.NoError(t, err)
	cliCert, err := pki.IssueClientCert(ca.CertPEM, ca.KeyPEM, "", time.Hour)
	require.NoError(t, err)

	certDir := filepath.Join(tmp, "certs")
	require.NoError(t, os.MkdirAll(certDir, 0o700))
	caFile := filepath.Join(certDir, "ca.crt")
	certFile := filepath.Join(certDir, "tls.crt")
	keyFile := filepath.Join(certDir, "tls.key")
	require.NoError(t, os.WriteFile(caFile, ca.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(certFile, srvCert.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, srvCert.KeyPEM, 0o600))

	supervisor = rckube.NewSupervisor(stateDir, 2*time.Second, rckube.WithRoots(homeDir, workspaceDir, runDir))
	t.Cleanup(supervisor.Shutdown)

	srv := execdhttp.NewServer(supervisor, homeDir, workspaceDir, "agent")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeListener(ctx, ln, execdhttp.Config{
			CAFile:       caFile,
			CertFile:     certFile,
			KeyFile:      keyFile,
			HomeDir:      homeDir,
			WorkspaceDir: workspaceDir,
		})
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-errCh)
	})

	tlsCfg, err := pki.ClientTLSConfig(ca.CertPEM, cliCert.CertPEM, cliCert.KeyPEM, serverName)
	require.NoError(t, err)
	client = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsCfg,
			ForceAttemptHTTP2: true,
		},
	}
	return "https://" + ln.Addr().String(), client, supervisor, workspaceDir
}

func TestExecdPingAndMTLSRejection(t *testing.T) {
	baseURL, client, _, _ := startTestExecdServer(t)

	resp, err := client.Get(baseURL + "/ping")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, execd.Version, resp.Header.Get("X-Execd-Version"))

	noCertClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	_, err = noCertClient.Get(baseURL + "/ping")
	require.Error(t, err)
}

func TestExecdCommandsSSEBackgroundStatusAndLogs(t *testing.T) {
	baseURL, client, supervisor, _ := startTestExecdServer(t)

	// 1. Foreground SSE streaming command.
	fgCmd := "echo hello-stdout; echo hello-stderr >&2"
	bgCmd := "echo line1; echo line2; sleep 30"
	if runtime.GOOS == "windows" {
		fgCmd = "echo hello-stdout & echo hello-stderr 1>&2"
		bgCmd = "echo line1 & echo line2 & ping -n 30 127.0.0.1 >nul"
	}
	resp, err := client.Post(baseURL+"/command", "application/json", strings.NewReader(
		`{"command": "`+fgCmd+`", "envs": {"FOO": "bar"}}`,
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	var events []map[string]any
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var ev map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev))
			events = append(events, ev)
		}
	}
	_ = resp.Body.Close()
	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, "init", events[0]["type"])
	assert.Equal(t, "execution_complete", events[len(events)-1]["type"])

	// 2. Background command -> status -> incremental cursor logs -> interrupt.
	bgResp, err := client.Post(baseURL+"/command", "application/json", strings.NewReader(
		`{"command": "`+bgCmd+`", "background": true}`,
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, bgResp.StatusCode)
	var bgID string
	bgScanner := bufio.NewScanner(bgResp.Body)
	for bgScanner.Scan() {
		line := bgScanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var ev map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev))
			if ev["type"] == "init" {
				bgID, _ = ev["text"].(string)
			}
		}
	}
	_ = bgResp.Body.Close()
	require.NotEmpty(t, bgID)

	// Poll logs with cursor until both lines appear.
	var logsText string
	var cursor string
	require.Eventually(t, func() bool {
		logResp, logErr := client.Get(baseURL + "/command/" + bgID + "/logs")
		if logErr != nil || logResp.StatusCode != http.StatusOK {
			return false
		}
		b, _ := io.ReadAll(logResp.Body)
		cursor = logResp.Header.Get("EXECD-COMMANDS-TAIL-CURSOR")
		_ = logResp.Body.Close()
		logsText = string(b)
		return strings.Contains(logsText, "line1") && strings.Contains(logsText, "line2")
	}, 3*time.Second, 50*time.Millisecond)
	assert.NotEmpty(t, cursor)

	// Status should report running=true.
	stResp, err := client.Get(baseURL + "/command/status/" + bgID)
	require.NoError(t, err)
	var status map[string]any
	require.NoError(t, json.NewDecoder(stResp.Body).Decode(&status))
	_ = stResp.Body.Close()
	assert.Equal(t, true, status["running"])
	assert.False(t, supervisor.LastActivity().IsZero())

	// Interrupt background command.
	delReq, _ := http.NewRequest(http.MethodDelete, baseURL+"/command?id="+bgID, nil)
	delResp, err := client.Do(delReq)
	require.NoError(t, err)
	_ = delResp.Body.Close()
	assert.Equal(t, http.StatusOK, delResp.StatusCode)
}

func TestExecdFilesAndDirectories(t *testing.T) {
	baseURL, client, _, workspaceDir := startTestExecdServer(t)

	// 1. Make directory.
	mkResp, err := client.Post(baseURL+"/directories", "application/json", strings.NewReader(`{"src":{"mode":755}}`))
	require.NoError(t, err)
	_ = mkResp.Body.Close()
	require.Equal(t, http.StatusOK, mkResp.StatusCode)

	// 2. Upload file via multipart metadata + file parts.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	metaPart, err := mw.CreateFormField("metadata")
	require.NoError(t, err)
	_, err = metaPart.Write([]byte(`{"path":"src/app.txt","mode":644}`))
	require.NoError(t, err)
	filePart, err := mw.CreateFormFile("file", "app.txt")
	require.NoError(t, err)
	_, err = filePart.Write([]byte("line1 hello\nline2 world\nline3 hello\n"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	upReq, _ := http.NewRequest(http.MethodPost, baseURL+"/files/upload", &body)
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upResp, err := client.Do(upReq)
	require.NoError(t, err)
	_ = upResp.Body.Close()
	require.Equal(t, http.StatusOK, upResp.StatusCode)

	// 3. Search & Replace (verbose=true).
	searchResp, err := client.Get(baseURL + "/files/search?path=src&pattern=*.txt")
	require.NoError(t, err)
	var found []map[string]any
	require.NoError(t, json.NewDecoder(searchResp.Body).Decode(&found))
	_ = searchResp.Body.Close()
	require.Len(t, found, 1)
	assert.Equal(t, filepath.Join(workspaceDir, "src", "app.txt"), found[0]["path"])

	repResp, err := client.Post(baseURL+"/files/replace?verbose=true", "application/json", strings.NewReader(`{
		"src/app.txt": {"old": "hello", "new": "hi"}
	}`))
	require.NoError(t, err)
	var repResult map[string]map[string]int
	require.NoError(t, json.NewDecoder(repResp.Body).Decode(&repResult))
	_ = repResp.Body.Close()
	assert.Equal(t, 2, repResult["src/app.txt"]["replacedCount"])

	// 4. Line-based download (offset=2&limit=2).
	dlResp, err := client.Get(baseURL + "/files/download?path=src/app.txt&offset=2&limit=2")
	require.NoError(t, err)
	dlBytes, _ := io.ReadAll(dlResp.Body)
	_ = dlResp.Body.Close()
	assert.Equal(t, "line2 world\nline3 hi\n", string(dlBytes))

	// 5. List directory.
	lsResp, err := client.Get(baseURL + "/directories/list?path=src&depth=1")
	require.NoError(t, err)
	var entries []map[string]any
	require.NoError(t, json.NewDecoder(lsResp.Body).Decode(&entries))
	_ = lsResp.Body.Close()
	require.Len(t, entries, 1)
	assert.Equal(t, "file", entries[0]["type"])
}
