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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

const protocolCodeBadRequest = "bad_request"

// Server exposes one Supervisor over a local Unix socket.
type Server struct {
	supervisor *Supervisor
}

// NewServer creates a local protocol server.
func NewServer(supervisor *Supervisor) *Server {
	return &Server{supervisor: supervisor}
}

// Serve accepts local protocol requests until ctx is cancelled.
func (server *Server) Serve(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create rc-kube socket directory: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale rc-kube socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on rc-kube Unix socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("restrict rc-kube Unix socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept rc-kube connection: %w", err)
		}
		go server.handle(ctx, connection)
	}
}

func (server *Server) handle(serverContext context.Context, connection net.Conn) {
	defer func() { _ = connection.Close() }()
	reader := bufio.NewReader(connection)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = writeProtocolResponse(connection, protocolResponse{Error: err.Error(), Code: protocolCodeBadRequest})
		return
	}
	request := new(protocolRequest)
	if err := json.Unmarshal(line, request); err != nil {
		_ = writeProtocolResponse(connection, protocolResponse{Error: err.Error(), Code: protocolCodeBadRequest})
		return
	}
	if request.Version != protocolVersion {
		_ = writeProtocolResponse(connection, protocolResponse{Error: "unsupported protocol version", Code: "version"})
		return
	}

	switch request.Action {
	case "start":
		if request.Start == nil {
			_ = writeProtocolResponse(connection, protocolResponse{Error: "start request is required", Code: protocolCodeBadRequest})
			return
		}
		state, err := server.supervisor.Start(*request.Start)
		server.writeState(connection, state, err)
	case "inspect":
		state, err := server.supervisor.Inspect(request.ID)
		server.writeState(connection, state, err)
	case "stop":
		state, err := server.supervisor.Stop(request.ID)
		server.writeState(connection, state, err)
	case "resize":
		err := server.supervisor.Resize(request.ID, request.ClientID, request.Rows, request.Columns)
		server.writeState(connection, processruntime.State{}, err)
	case "logs":
		if err := server.supervisor.TranscriptAvailable(request.ID); err != nil {
			server.writeState(connection, processruntime.State{}, err)
			return
		}
		if err := writeProtocolResponse(connection, protocolResponse{}); err != nil {
			return
		}
		_ = server.supervisor.Logs(request.ID, connection)
	case "attach":
		state, err := server.supervisor.Inspect(request.ID)
		if err != nil {
			server.writeState(connection, state, err)
			return
		}
		if err := writeProtocolResponse(connection, protocolResponse{State: state}); err != nil {
			return
		}
		attachContext, cancel := context.WithCancel(serverContext)
		defer cancel()
		if err := server.supervisor.Attach(attachContext, request.ID, request.ClientID, &cancelOnEOFReader{reader: reader, cancel: cancel}, connection, request.Rows, request.Columns); errors.Is(err, ErrSlowClient) {
			_, _ = fmt.Fprintf(connection, "\r\n[rc-kube: %s]\r\n", err)
		}
	default:
		_ = writeProtocolResponse(connection, protocolResponse{Error: "unknown action", Code: protocolCodeBadRequest})
	}
}

func (server *Server) writeState(connection net.Conn, state processruntime.State, err error) {
	if err == nil {
		_ = writeProtocolResponse(connection, protocolResponse{State: state})
		return
	}
	code := "internal"
	if errors.Is(err, processruntime.ErrNotFound) {
		code = "not_found"
	} else if errors.Is(err, ErrProcessConflict) {
		code = "conflict"
	}
	_ = writeProtocolResponse(connection, protocolResponse{Error: err.Error(), Code: code})
}

func writeProtocolResponse(connection net.Conn, response protocolResponse) error {
	response.Version = protocolVersion
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = connection.Write(data)

	return err
}

type cancelOnEOFReader struct {
	reader *bufio.Reader
	cancel context.CancelFunc
}

func (reader *cancelOnEOFReader) Read(data []byte) (int, error) {
	count, err := reader.reader.Read(data)
	if err != nil {
		reader.cancel()
	}

	return count, err
}
