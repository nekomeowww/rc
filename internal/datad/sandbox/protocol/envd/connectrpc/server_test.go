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

package envdconnectrpc_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fspb "github.com/nekomeowww/rc/apis/sdk/go/envd/filesystem"
	"github.com/nekomeowww/rc/apis/sdk/go/envd/filesystem/filesystemconnect"
	procpb "github.com/nekomeowww/rc/apis/sdk/go/envd/process"
	"github.com/nekomeowww/rc/apis/sdk/go/envd/process/processconnect"
	"github.com/nekomeowww/rc/internal/datad/pki"
	envdconnectrpc "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/envd/connectrpc"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/pkg/envd"
)

func startTestEnvdServer(t *testing.T) (baseURL string, client *http.Client, supervisor *rckube.Supervisor, homeDir string) {
	t.Helper()
	tmp := t.TempDir()
	homeDir = filepath.Join(tmp, "home")
	workspaceDir := filepath.Join(tmp, "workspace")
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

	srv := envdconnectrpc.NewServer(supervisor, homeDir, "agent")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeListener(ctx, ln, envdconnectrpc.Config{
			CAFile:   caFile,
			CertFile: certFile,
			KeyFile:  keyFile,
			HomeDir:  homeDir,
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
	return "https://" + ln.Addr().String(), client, supervisor, homeDir
}

func TestEnvdHealthInitAndMTLSRejection(t *testing.T) {
	baseURL, client, _, _ := startTestEnvdServer(t)

	resp, err := client.Get(baseURL + "/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, envd.Version, resp.Header.Get("X-Envd-Version"))

	initResp, err := client.Post(baseURL+"/init", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer func() { _ = initResp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, initResp.StatusCode)

	// Connection without client certificate must be refused by mTLS.
	noCertClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	_, err = noCertClient.Get(baseURL + "/health")
	require.Error(t, err)
}

func TestEnvdFilesRESTAndFilesystemRPC(t *testing.T) {
	baseURL, client, _, homeDir := startTestEnvdServer(t)

	// 1. Multipart multi-file upload with metadata.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p1, err := mw.CreateFormFile("file", "nested/hello.txt")
	require.NoError(t, err)
	_, err = p1.Write([]byte("hello world"))
	require.NoError(t, err)
	p2, err := mw.CreateFormFile("file", "nested/second.txt")
	require.NoError(t, err)
	_, err = p2.Write([]byte("second file"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, baseURL+"/files", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Metadata-Source", "unit-test")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var uploaded []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&uploaded))
	_ = resp.Body.Close()
	require.Len(t, uploaded, 2)

	// 2. Download with Accept-Encoding: gzip.
	dlReq, err := http.NewRequest(http.MethodGet, baseURL+"/files?path=nested/hello.txt", nil)
	require.NoError(t, err)
	dlReq.Header.Set("Accept-Encoding", "gzip")
	dlResp, err := client.Do(dlReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, dlResp.StatusCode)
	assert.Equal(t, "gzip", dlResp.Header.Get("Content-Encoding"))
	gz, err := gzip.NewReader(dlResp.Body)
	require.NoError(t, err)
	gotBytes, err := io.ReadAll(gz)
	require.NoError(t, err)
	_ = gz.Close()
	_ = dlResp.Body.Close()
	assert.Equal(t, "hello world", string(gotBytes))

	// 3. Reject root user.
	rootResp, err := client.Get(baseURL + "/files?path=nested/hello.txt&username=root")
	require.NoError(t, err)
	_ = rootResp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, rootResp.StatusCode)

	// 4. ConnectRPC Filesystem: Stat, ListDir, MakeDir, Move, Remove, WatchDir.
	fsClient := filesystemconnect.NewFilesystemClient(connect.NewClient(connecthttp.NewTransport(client, baseURL)))
	ctx := context.Background()

	statResp, err := fsClient.Stat(ctx, &fspb.StatRequest{Path: "~/nested/hello.txt"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(homeDir, "nested", "hello.txt"), statResp.Entry.Path)
	assert.Equal(t, "unit-test", statResp.Entry.Metadata["source"])

	// Reject root via Basic auth header on ConnectRPC.
	rootCtx, rootCallInfo := connect.NewClientContext(ctx)
	rootCallInfo.RequestHeader().Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("root:")))
	_, err = fsClient.Stat(rootCtx, &fspb.StatRequest{Path: "~/nested/hello.txt"})
	require.Error(t, err)
	assert.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	_, err = fsClient.MakeDir(ctx, &fspb.MakeDirRequest{Path: "watched"})
	require.NoError(t, err)

	cwResp, err := fsClient.CreateWatcher(ctx, &fspb.CreateWatcherRequest{
		Path:         "watched",
		IncludeEntry: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, cwResp.WatcherId)

	require.NoError(t, os.WriteFile(filepath.Join(homeDir, "watched", "ping.txt"), []byte("pong"), 0o644))
	require.Eventually(t, func() bool {
		evResp, evErr := fsClient.GetWatcherEvents(ctx, &fspb.GetWatcherEventsRequest{
			WatcherId: cwResp.WatcherId,
		})
		return evErr == nil && len(evResp.Events) > 0
	}, 3*time.Second, 50*time.Millisecond)

	_, err = fsClient.RemoveWatcher(ctx, &fspb.RemoveWatcherRequest{WatcherId: cwResp.WatcherId})
	require.NoError(t, err)

	listResp, err := fsClient.ListDir(ctx, &fspb.ListDirRequest{Path: "nested", Depth: 1})
	require.NoError(t, err)
	assert.Len(t, listResp.Entries, 2)

	_, err = fsClient.Move(ctx, &fspb.MoveRequest{
		Source:      "nested/second.txt",
		Destination: "nested/moved.txt",
	})
	require.NoError(t, err)

	_, err = fsClient.Remove(ctx, &fspb.RemoveRequest{Path: "nested/moved.txt"})
	require.NoError(t, err)
}

func TestEnvdProcessRPCAndActivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell utilities (/bin/sh, /bin/cat)")
	}
	baseURL, client, supervisor, _ := startTestEnvdServer(t)
	procClient := processconnect.NewProcessClient(connect.NewClient(connecthttp.NewTransport(client, baseURL)))
	ctx := context.Background()

	// 1. Start process with separate stdout and stderr.
	stream, err := procClient.Start(ctx, &procpb.StartRequest{
		Process: &procpb.ProcessConfig{
			Cmd:  "/bin/sh",
			Args: []string{"-c", "echo out-line; echo err-line >&2"},
		},
	})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	var stdout, stderr bytes.Buffer
	var pid uint32
	var endEvent *procpb.ProcessEvent_EndEvent
	for {
		msg, recvErr := stream.Receive()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		require.NoError(t, recvErr)
		ev := msg.GetEvent()
		switch e := ev.Event.(type) {
		case *procpb.ProcessEvent_Start:
			pid = e.Start.Pid
		case *procpb.ProcessEvent_Data:
			stdout.Write(e.Data.GetStdout())
			stderr.Write(e.Data.GetStderr())
		case *procpb.ProcessEvent_End:
			endEvent = e.End
		}
	}
	assert.NotZero(t, pid)
	assert.Equal(t, "out-line\n", stdout.String())
	assert.Equal(t, "err-line\n", stderr.String())
	require.NotNil(t, endEvent)
	assert.Equal(t, int32(0), endEvent.ExitCode)
	assert.True(t, endEvent.Exited)

	// Verify supervisor recorded the envd_ process and activity.
	entries, err := supervisor.List()
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	assert.True(t, strings.HasPrefix(entries[0].ID, "envd_"))
	assert.False(t, supervisor.LastActivity().IsZero())

	// 2. Interactive stdin + CloseStdin.
	stdinTrue := true
	tag := "cat-test"
	catStream, err := procClient.Start(ctx, &procpb.StartRequest{
		Process: &procpb.ProcessConfig{Cmd: "/bin/cat"},
		Tag:     &tag,
		Stdin:   &stdinTrue,
	})
	require.NoError(t, err)
	defer func() { _ = catStream.Close() }()
	firstCatMsg, err := catStream.Receive()
	require.NoError(t, err)
	catPID := firstCatMsg.GetEvent().GetStart().GetPid()
	require.NotZero(t, catPID)

	_, err = procClient.SendInput(ctx, &procpb.SendInputRequest{
		Process: &procpb.ProcessSelector{Selector: &procpb.ProcessSelector_Pid{Pid: catPID}},
		Input:   &procpb.ProcessInput{Input: &procpb.ProcessInput_Stdin{Stdin: []byte("hello stdin\n")}},
	})
	require.NoError(t, err)
	_, err = procClient.CloseStdin(ctx, &procpb.CloseStdinRequest{
		Process: &procpb.ProcessSelector{Selector: &procpb.ProcessSelector_Tag{Tag: tag}},
	})
	require.NoError(t, err)

	var catOut bytes.Buffer
	for {
		msg, recvErr := catStream.Receive()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		require.NoError(t, recvErr)
		if d := msg.GetEvent().GetData(); d != nil {
			catOut.Write(d.GetStdout())
		}
	}
	assert.Equal(t, "hello stdin\n", catOut.String())

	// 3. PTY process + resize + signal.
	ptyStream, err := procClient.Start(ctx, &procpb.StartRequest{
		Process: &procpb.ProcessConfig{Cmd: "/bin/sh", Args: []string{"-c", "sleep 30"}},
		Pty:     &procpb.PTY{Size: &procpb.PTY_Size{Cols: 80, Rows: 24}},
	})
	require.NoError(t, err)
	defer func() { _ = ptyStream.Close() }()
	firstPtyMsg, err := ptyStream.Receive()
	require.NoError(t, err)
	ptyPID := firstPtyMsg.GetEvent().GetStart().GetPid()
	require.NotZero(t, ptyPID)

	listResp, err := procClient.List(ctx, &procpb.ListRequest{})
	require.NoError(t, err)
	require.Len(t, listResp.Processes, 1)
	assert.Equal(t, ptyPID, listResp.Processes[0].Pid)

	_, err = procClient.Update(ctx, &procpb.UpdateRequest{
		Process: &procpb.ProcessSelector{Selector: &procpb.ProcessSelector_Pid{Pid: ptyPID}},
		Pty:     &procpb.PTY{Size: &procpb.PTY_Size{Cols: 120, Rows: 40}},
	})
	require.NoError(t, err)

	_, err = procClient.SendSignal(ctx, &procpb.SendSignalRequest{
		Process: &procpb.ProcessSelector{Selector: &procpb.ProcessSelector_Pid{Pid: ptyPID}},
		Signal:  procpb.Signal_SIGNAL_SIGKILL,
	})
	require.NoError(t, err)
	for {
		_, recvErr := ptyStream.Receive()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		require.NoError(t, recvErr)
	}
}
