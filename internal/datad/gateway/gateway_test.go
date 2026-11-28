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

package gateway_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	procpb "github.com/nekomeowww/rc/apis/sdk/go/envd/process"
	"github.com/nekomeowww/rc/apis/sdk/go/envd/process/processconnect"
	"github.com/nekomeowww/rc/internal/datad/gateway"
	"github.com/nekomeowww/rc/internal/datad/pki"
	envdconnectrpc "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/envd/connectrpc"
	execdhttp "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/execd/http"
	"github.com/nekomeowww/rc/internal/rckube"
)

type mapResolver struct {
	mu      sync.RWMutex
	targets map[string]gateway.Target
}

func (r *mapResolver) Resolve(_ context.Context, namespace, name string) (gateway.Target, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.targets[namespace+"/"+name]
	if !ok {
		return gateway.Target{}, gateway.ErrNotFound
	}
	return t, nil
}

func TestGatewayRoutingAuthRotationAndStreaming(t *testing.T) {
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))

	ca, err := pki.GenerateCA(time.Hour)
	require.NoError(t, err)
	serverName := pki.WorkspaceServerName("team-a", "ws-1")
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

	supervisor := rckube.NewSupervisor(filepath.Join(tmp, "state"), 2*time.Second, rckube.WithRoots(homeDir, tmp, filepath.Join(tmp, "run")))
	t.Cleanup(supervisor.Shutdown)

	dpServer := envdconnectrpc.NewServer(supervisor, homeDir, "agent")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	execdServer := execdhttp.NewServer(supervisor, homeDir, homeDir, "agent")
	execdLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- dpServer.ServeListener(ctx, ln, envdconnectrpc.Config{
			CAFile:   caFile,
			CertFile: certFile,
			KeyFile:  keyFile,
			HomeDir:  homeDir,
		})
	}()
	execdErrCh := make(chan error, 1)
	go func() {
		execdErrCh <- execdServer.ServeListener(ctx, execdLn, execdhttp.Config{
			CAFile:       caFile,
			CertFile:     certFile,
			KeyFile:      keyFile,
			HomeDir:      homeDir,
			WorkspaceDir: homeDir,
		})
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-errCh)
		require.NoError(t, <-execdErrCh)
	})

	resolver := &mapResolver{
		targets: map[string]gateway.Target{
			"team-a/ws-1": {
				Namespace:    "team-a",
				Name:         "ws-1",
				Ready:        true,
				State:        "Running",
				ExecdAddress: execdLn.Addr().String(),
				Address:      ln.Addr().String(),
				ServerName:   serverName,
				AccessToken:  "secret-token-123456789012345678901234567890",
			},
			"team-a/suspended": {
				Namespace:   "team-a",
				Name:        "suspended",
				Ready:       false,
				State:       "Suspended",
				AccessToken: "secret-token-123456789012345678901234567890",
			},
		},
	}

	gw := gateway.NewServer(resolver, func(_ context.Context) ([]byte, []byte, []byte, error) {
		return ca.CertPEM, cliCert.CertPEM, cliCert.KeyPEM, nil
	})
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)

	wsBase := ts.URL + "/namespaces/team-a/workspaces/ws-1"

	// 404 for unknown workspace.
	resp, err := http.Get(ts.URL + "/namespaces/team-a/workspaces/missing/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// 503 for suspended workspace.
	suspReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/namespaces/team-a/workspaces/suspended/files?path=a.txt", nil)
	suspReq.Header.Set("X-Access-Token", "secret-token-123456789012345678901234567890")
	resp, err = http.DefaultClient.Do(suspReq)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	// 204 for /health without token.
	resp, err = http.Get(wsBase + "/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// 401 without token on /files.
	resp, err = http.Get(wsBase + "/files?path=hello.txt")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Upload with valid token.
	upReq, _ := http.NewRequest(http.MethodPost, wsBase+"/files?path=hello.txt", strings.NewReader("from gateway"))
	upReq.Header.Set("Content-Type", "application/octet-stream")
	upReq.Header.Set("X-Access-Token", "secret-token-123456789012345678901234567890")
	resp, err = http.DefaultClient.Do(upReq)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Download with signed URL (no X-Access-Token header).
	exp := time.Now().Add(time.Minute).Unix()
	sig := gateway.SignFileURL("hello.txt", "read", "user", "secret-token-123456789012345678901234567890", exp)
	resp, err = http.Get(fmt.Sprintf("%s/files?path=hello.txt&signature=%s&signature_expiration=%d", wsBase, sig, exp))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "from gateway", string(body))

	// Token rotation takes effect immediately.
	resolver.mu.Lock()
	rotated := resolver.targets["team-a/ws-1"]
	rotated.AccessToken = "rotated-token-987654321098765432109876543210"
	resolver.targets["team-a/ws-1"] = rotated
	resolver.mu.Unlock()

	oldReq, _ := http.NewRequest(http.MethodGet, wsBase+"/files?path=hello.txt", nil)
	oldReq.Header.Set("X-Access-Token", "secret-token-123456789012345678901234567890")
	resp, err = http.DefaultClient.Do(oldReq)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Streaming ConnectRPC Process.Start through the gateway with rotated token.
	procClient := processconnect.NewProcessClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, wsBase)))
	shellCmd, shellArgs := "/bin/sh", []string{"-c", "echo streamed-through-gateway"}
	if runtime.GOOS == "windows" {
		shellCmd, shellArgs = "cmd.exe", []string{"/C", "echo streamed-through-gateway"}
	}
	startCtx, callInfo := connect.NewClientContext(context.Background())
	callInfo.RequestHeader().Set("X-Access-Token", "rotated-token-987654321098765432109876543210")
	stream, err := procClient.Start(startCtx, &procpb.StartRequest{
		Process: &procpb.ProcessConfig{
			Cmd:  shellCmd,
			Args: shellArgs,
		},
	})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	var out strings.Builder
	for {
		msg, recvErr := stream.Receive()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		require.NoError(t, recvErr)
		if d := msg.GetEvent().GetData(); d != nil {
			out.Write(d.GetStdout())
		}
	}
	assert.Contains(t, out.String(), "streamed-through-gateway")

	// Primary execd route through the same gateway with X-EXECD-ACCESS-TOKEN over mTLS.
	pingResp, err := http.Get(wsBase + "/execd/ping")
	require.NoError(t, err)
	_ = pingResp.Body.Close()
	assert.Equal(t, http.StatusOK, pingResp.StatusCode)

	execdReq, _ := http.NewRequest(http.MethodPost, wsBase+"/execd/command", strings.NewReader(`{"command":"echo via-execd-gateway"}`))
	execdReq.Header.Set("Content-Type", "application/json")
	execdReq.Header.Set("X-EXECD-ACCESS-TOKEN", "rotated-token-987654321098765432109876543210")
	execdResp, err := http.DefaultClient.Do(execdReq)
	require.NoError(t, err)
	execdBody, _ := io.ReadAll(execdResp.Body)
	_ = execdResp.Body.Close()
	require.Equal(t, http.StatusOK, execdResp.StatusCode)
	assert.Contains(t, string(execdBody), "via-execd-gateway")
}
