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
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
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
	server := NewServer(NewSupervisor(t.TempDir(), 100*time.Millisecond))
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
	_, err = client.Start(context.Background(), request)
	requirements.NoError(err, "start through Unix protocol")

	client = NewClient(socketPath)
	requirements.Eventually(func() bool {
		state, inspectErr := client.Inspect(context.Background(), request.ID)
		return inspectErr == nil && state.Phase == phaseExited
	}, 3*time.Second, 10*time.Millisecond, "detached process keeps running")

	var transcript bytes.Buffer
	requirements.NoError(client.Logs(context.Background(), request.ID, &transcript), "read logs through new client")
	assertions.Equal("done", transcript.String(), "new client sees detached output")

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
	supervisor := NewSupervisor(t.TempDir(), 100*time.Millisecond)
	request := processruntime.StartRequest{ID: "shared", UID: "shared-uid", Command: []string{"sh", "-c", "sleep 0.1; printf shared"}}
	_, err := supervisor.Start(request)
	requirements.NoError(err, "start shared process")

	var first bytes.Buffer
	var second bytes.Buffer
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_ = supervisor.Attach(context.Background(), request.ID, "first", nil, &first)
	}()
	go func() {
		defer wait.Done()
		_ = supervisor.Attach(context.Background(), request.ID, "second", nil, &second)
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
	server := NewServer(NewSupervisor(t.TempDir(), 100*time.Millisecond))
	go func() { _ = server.Serve(ctx, socketPath) }()
	requirements.Eventually(func() bool {
		return errors.Is(NewClient(socketPath).Logs(context.Background(), "missing", &bytes.Buffer{}), processruntime.ErrNotFound)
	}, time.Second, 10*time.Millisecond, "missing transcript is a protocol error")
}
