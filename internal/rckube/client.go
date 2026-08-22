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
	"io"
	"net"

	processruntime "github.com/nekomeowww/rc/internal/agentprocess"
)

// Client talks to one rc-kube Unix socket.
type Client struct {
	socketPath string
}

// NewClient creates a local protocol client.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

func (client *Client) Start(ctx context.Context, request processruntime.StartRequest) (processruntime.State, error) {
	return client.stateRequest(ctx, protocolRequest{Action: "start", Start: &request})
}

func (client *Client) Inspect(ctx context.Context, id string) (processruntime.State, error) {
	return client.stateRequest(ctx, protocolRequest{Action: "inspect", ID: id})
}

func (client *Client) Stop(ctx context.Context, id string) (processruntime.State, error) {
	return client.stateRequest(ctx, protocolRequest{Action: "stop", ID: id})
}

func (client *Client) Resize(ctx context.Context, id string, clientID string, rows uint16, columns uint16) error {
	_, err := client.stateRequest(ctx, protocolRequest{Action: "resize", ID: id, ClientID: clientID, Rows: rows, Columns: columns})
	return err
}

func (client *Client) Logs(ctx context.Context, id string, output io.Writer) error {
	connection, reader, response, err := client.open(ctx, protocolRequest{Action: "logs", ID: id})
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if err := responseError(response); err != nil {
		return err
	}
	_, err = io.Copy(output, reader)

	return err
}

func (client *Client) Attach(ctx context.Context, id string, clientID string, input io.Reader, output io.Writer) error {
	connection, reader, response, err := client.open(ctx, protocolRequest{Action: "attach", ID: id, ClientID: clientID})
	if err != nil {
		return err
	}
	defer func() { _ = connection.Close() }()
	if err := responseError(response); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()
	if input != nil {
		go func() { _, _ = io.Copy(connection, input) }()
	}
	_, err = io.Copy(output, reader)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return err
}

func (client *Client) stateRequest(ctx context.Context, request protocolRequest) (processruntime.State, error) {
	connection, _, response, err := client.open(ctx, request)
	if err != nil {
		return processruntime.State{}, err
	}
	defer func() { _ = connection.Close() }()
	if err := responseError(response); err != nil {
		return processruntime.State{}, err
	}

	return response.State, nil
}

func (client *Client) open(ctx context.Context, request protocolRequest) (net.Conn, *bufio.Reader, protocolResponse, error) {
	dialer := new(net.Dialer)
	connection, err := dialer.DialContext(ctx, "unix", client.socketPath)
	if err != nil {
		return nil, nil, protocolResponse{}, fmt.Errorf("connect to rc-kube: %w", err)
	}
	request.Version = protocolVersion
	data, err := json.Marshal(request)
	if err != nil {
		_ = connection.Close()
		return nil, nil, protocolResponse{}, fmt.Errorf("encode rc-kube request: %w", err)
	}
	if _, err := connection.Write(append(data, '\n')); err != nil {
		_ = connection.Close()
		return nil, nil, protocolResponse{}, fmt.Errorf("write rc-kube request: %w", err)
	}
	reader := bufio.NewReader(connection)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = connection.Close()
		return nil, nil, protocolResponse{}, fmt.Errorf("read rc-kube response: %w", err)
	}
	response := protocolResponse{}
	if err := json.Unmarshal(line, &response); err != nil {
		_ = connection.Close()
		return nil, nil, protocolResponse{}, fmt.Errorf("decode rc-kube response: %w", err)
	}
	if response.Version != protocolVersion {
		_ = connection.Close()
		return nil, nil, protocolResponse{}, fmt.Errorf("rc-kube protocol version %d is not supported", response.Version)
	}

	return connection, reader, response, nil
}

func responseError(response protocolResponse) error {
	if response.Error == "" {
		return nil
	}
	switch response.Code {
	case "not_found":
		return fmt.Errorf("%w: %s", processruntime.ErrNotFound, response.Error)
	case "conflict":
		return fmt.Errorf("%w: %s", ErrProcessConflict, response.Error)
	default:
		return errors.New(response.Error)
	}
}
