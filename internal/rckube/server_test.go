//go:build !windows

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

package rckube

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/execution"
)

func TestUnixServerKeepsProcessAfterClientDisconnect(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketFile, err := os.CreateTemp("/tmp", "rc-kube-*.sock")
	requirements.NoError(err, "reserve short Unix socket path")
	socketPath := socketFile.Name()
	requirements.NoError(socketFile.Close(), "close socket path placeholder")
	requirements.NoError(os.Remove(socketPath), "release socket path placeholder")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server := NewServer(newTestSupervisor(t, t.TempDir(), 100*time.Millisecond))
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ctx, socketPath) }()
	requirements.Eventually(func() bool {
		client := NewClient(socketPath)
		_, err := client.Inspect(context.Background(), "missing")
		return errors.Is(err, processruntime.ErrNotFound)
	}, time.Second, 10*time.Millisecond, "wait for Unix server")

	client := NewClient(socketPath)
	request := processruntime.StartRequest{
		ID: "detached", UID: "detached-uid", Command: []string{"sh", "-c", "sleep 0.1; printf done"},
	}
	startContext, cancelStart := context.WithCancel(t.Context())
	defer cancelStart()
	_, err = client.Start(startContext, request)
	requirements.NoError(err, "start through Unix protocol")
	cancelStart()

	client = NewClient(socketPath)
	requirements.Eventually(func() bool {
		state, inspectErr := client.Inspect(context.Background(), request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "detached process keeps running")

	var transcript bytes.Buffer
	requirements.NoError(client.Logs(context.Background(), request.ID, &transcript), "read logs through new client")
	assertions.Equal("done", transcript.String(), "new client sees detached output")
	_, err = client.Start(context.Background(), request)
	requirements.NoError(err, "a protocol replay returns the original process instead of starting again")

	cancel()
	select {
	case err := <-serverDone:
		requirements.NoError(err, "stop Unix server")
	case <-time.After(time.Second):
		requirements.Fail("Unix server did not stop")
	}
}

func TestSupervisorSupportsConcurrentReadOnlyAttaches(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	supervisor := newTestSupervisor(t, t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{ID: "shared", UID: "shared-uid", Command: []string{"sh", "-c", "sleep 0.1; printf shared"}}
	_, err := supervisor.Start(t.Context(), request)
	requirements.NoError(err, "start shared process")

	var first bytes.Buffer
	var second bytes.Buffer
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_ = supervisor.Attach(context.Background(), request.ID, "first", nil, &first, processruntime.AttachOptions{})
	}()
	go func() {
		defer wait.Done()
		_ = supervisor.Attach(context.Background(), request.ID, "second", nil, &second, processruntime.AttachOptions{})
	}()
	wait.Wait()
	assertions.Equal("shared", first.String(), "first client receives terminal output")
	assertions.Equal("shared", second.String(), "second client receives terminal output")
}

func TestUnixServerReportsMissingLogs(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	ctx := t.Context()
	socketFile, err := os.CreateTemp("/tmp", "rc-kube-*.sock")
	requirements.NoError(err, "reserve short Unix socket path")
	socketPath := socketFile.Name()
	requirements.NoError(socketFile.Close(), "close socket path placeholder")
	requirements.NoError(os.Remove(socketPath), "release socket path placeholder")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	server := NewServer(newTestSupervisor(t, t.TempDir(), 100*time.Millisecond))
	go func() { _ = server.Serve(ctx, socketPath) }()
	requirements.Eventually(func() bool {
		return errors.Is(NewClient(socketPath).Logs(context.Background(), "missing", &bytes.Buffer{}), processruntime.ErrNotFound)
	}, time.Second, 10*time.Millisecond, "missing transcript is a protocol error")
}

func TestUnixServerListsRecordedProcesses(t *testing.T) {
	t.Parallel()
	requirements := require.New(t)
	ctx := t.Context()
	socketFile, err := os.CreateTemp("/tmp", "rc-kube-*.sock")
	requirements.NoError(err, "reserve short Unix socket path")
	socketPath := socketFile.Name()
	requirements.NoError(socketFile.Close(), "close socket path placeholder")
	requirements.NoError(os.Remove(socketPath), "release socket path placeholder")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	supervisor := newTestSupervisor(t, t.TempDir(), 100*time.Millisecond)
	go func() { _ = NewServer(supervisor).Serve(ctx, socketPath) }()
	client := NewClient(socketPath)
	requirements.Eventually(func() bool { return client.Ping(context.Background()) == nil }, time.Second, 10*time.Millisecond)
	request := processruntime.StartRequest{ID: "listed", UID: testProcessUID, Command: []string{testTrueCommand}}
	_, err = client.Start(ctx, request)
	requirements.NoError(err)
	waitForExit(t, supervisor, request.ID)

	entries, err := client.List(ctx)
	requirements.NoError(err)
	requirements.Len(entries, 1)
	requirements.Equal(request.ID, entries[0].ID)
	requirements.Equal(request.Command, entries[0].Command)
	requirements.Equal(phaseExited, entries[0].Phase)

	requirements.NoError(client.PruneHistory(ctx, processruntime.HistoryPolicy{MaxEntries: 1}), "keep the newest record")
	entries, err = client.List(ctx)
	requirements.NoError(err)
	requirements.Len(entries, 1)
	requirements.NoError(client.PruneHistory(ctx, processruntime.HistoryPolicy{TTLAfterFinished: time.Nanosecond}))
	entries, err = client.List(ctx)
	requirements.NoError(err)
	requirements.Empty(entries, "prune through the protocol")
}

func TestClientCancellationInterruptsResponseRead(t *testing.T) {
	t.Parallel()
	socketFile, err := os.CreateTemp("/tmp", "rc-context-*.sock")
	require.NoError(t, err)
	address := socketFile.Name()
	require.NoError(t, socketFile.Close())
	require.NoError(t, os.Remove(address))
	listener, err := listenLocal(address)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close(); _ = os.Remove(address) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		_, readErr := bufio.NewReader(connection).ReadBytes('\n')
		cancel()
		if readErr != nil {
			done <- readErr
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = connection.Read(make([]byte, 1))
		done <- nil
	}()
	err = NewClient(address).Ping(ctx)
	require.NoError(t, <-done, "complete the peer's request handling")
	require.ErrorIs(t, err, context.Canceled, "cancellation must interrupt reads after dialing")
}

func startTestServer(t *testing.T, options ...SupervisorOption) (*Supervisor, *Client) {
	t.Helper()
	socketFile, err := os.CreateTemp("/tmp", "rc-kube-*.sock")
	require.NoError(t, err)
	socketPath := socketFile.Name()
	require.NoError(t, socketFile.Close())
	require.NoError(t, os.Remove(socketPath))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	supervisor := newTestSupervisor(t, t.TempDir(), 100*time.Millisecond, options...)
	go func() { _ = NewServer(supervisor).Serve(t.Context(), socketPath) }()
	client := NewClient(socketPath)
	require.Eventually(t, func() bool { return client.Ping(context.Background()) == nil }, time.Second, 10*time.Millisecond)

	return supervisor, client
}

func TestProcessWithoutStdinReadsEndOfFile(t *testing.T) {
	t.Parallel()
	supervisor, client := startTestServer(t)
	request := processruntime.StartRequest{ID: "no-stdin", UID: testProcessUID, Command: []string{"sh", "-c", "cat; echo finished"}}
	_, err := client.Start(t.Context(), request)
	require.NoError(t, err)
	waitForExit(t, supervisor, request.ID)
	var transcript bytes.Buffer
	require.NoError(t, client.Logs(t.Context(), request.ID, &transcript))
	require.Equal(t, "finished\n", transcript.String(), "never wait on input no client will send")
}

func TestAttachClosesInputAtEndOfFile(t *testing.T) {
	t.Parallel()
	_, client := startTestServer(t)
	request := processruntime.StartRequest{ID: "pipe", UID: testProcessUID, Command: []string{"sh", "-c", "cat; echo finished"}, Stdin: true}
	_, err := client.Start(t.Context(), request)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	var output bytes.Buffer
	err = client.Attach(ctx, request.ID, "pipe", bytes.NewReader(input), &output, processruntime.AttachOptions{Lossless: true, CloseInputOnEOF: true})
	require.NoError(t, err, "the process sees end of file and exits")
	require.Equal(t, string(input)+"finished\n", output.String(), "deliver every input byte before closing input")
}

type slowWriter struct {
	written int
}

func (writer *slowWriter) Write(data []byte) (int, error) {
	time.Sleep(time.Millisecond)
	writer.written += len(data)
	return len(data), nil
}

func TestLosslessAttachDeliversEveryByteToASlowReader(t *testing.T) {
	t.Parallel()
	_, client := startTestServer(t)
	const size = 20 << 20
	request := processruntime.StartRequest{ID: "bulk", UID: testProcessUID, Command: []string{"sh", "-c", fmt.Sprintf("head -c %d /dev/zero", size)}}
	_, err := client.Start(t.Context(), request)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	reader := &slowWriter{}
	require.NoError(t, client.Attach(ctx, request.ID, "bulk", nil, reader, processruntime.AttachOptions{Lossless: true}))
	require.Equal(t, size, reader.written, "a slow lossless client receives every byte")
}

func TestTruncatedReplayIsReportedNotSilent(t *testing.T) {
	t.Parallel()
	const limit = 1024
	supervisor, client := startTestServer(t, WithMaxTranscriptBytes(limit))
	request := processruntime.StartRequest{ID: "flood", UID: testProcessUID, Command: []string{"sh", "-c", "head -c 4096 /dev/zero"}}
	_, err := client.Start(t.Context(), request)
	require.NoError(t, err)
	waitForExit(t, supervisor, request.ID)

	var attached bytes.Buffer
	err = client.Attach(t.Context(), request.ID, "late", nil, &attached, processruntime.AttachOptions{Lossless: true})
	require.ErrorIs(t, err, processruntime.ErrTranscriptTruncated, "a late client learns that output is missing")
	require.Equal(t, bytes.Repeat([]byte{0}, limit), attached.Bytes(), "the replay carries output only, never the truncation notice")

	var logs bytes.Buffer
	require.ErrorIs(t, client.Logs(t.Context(), request.ID, &logs), processruntime.ErrTranscriptTruncated)
	require.Equal(t, limit, logs.Len())
}
