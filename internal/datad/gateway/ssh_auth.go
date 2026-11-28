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

package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	sshv1alpha1 "github.com/nekomeowww/rc/apis/sdk/go/datad/ssh/v1alpha1"
	datadssh "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/ssh"
)

const defaultGRPCAuthTimeout = 3 * time.Second

// SSHAuthRequest carries SSH handshake metadata and credentials for an
// external or built-in SSHAuthenticator.
type SSHAuthRequest struct {
	Username   string
	Authority  string
	RemoteAddr string
	Method     sshv1alpha1.SSHAuthMethod
	PublicKey  ssh.PublicKey
	Password   []byte
}

// SSHAuthResult identifies the local-cluster Workspace that an authenticated
// SSH connection should be routed to.
type SSHAuthResult struct {
	Namespace string
	Workspace string
	UserID    string
}

// SSHAuthenticator validates an SSH client's credentials and maps the request
// (username, TLS SNI authority, key/password) to a local-cluster Workspace.
type SSHAuthenticator interface {
	Authenticate(ctx context.Context, req SSHAuthRequest) (SSHAuthResult, error)
}

// GRPCSSHAuthenticator delegates SSH authentication and Workspace routing to
// an external gRPC plugin implementing rc.datad.ssh.v1alpha1.SSHAuthService.
type GRPCSSHAuthenticator struct {
	client  sshv1alpha1.SSHAuthServiceClient
	timeout time.Duration
}

// NewGRPCSSHAuthenticator creates a gRPC-backed SSHAuthenticator with the
// given client and per-RPC timeout (defaults to 3s).
func NewGRPCSSHAuthenticator(client sshv1alpha1.SSHAuthServiceClient, timeout time.Duration) *GRPCSSHAuthenticator {
	if timeout <= 0 {
		timeout = defaultGRPCAuthTimeout
	}
	return &GRPCSSHAuthenticator{
		client:  client,
		timeout: timeout,
	}
}

// DialGRPCSSHAuthenticator dials target and returns a GRPCSSHAuthenticator
// together with the underlying gRPC ClientConn (which the caller must close).
// When caFile is non-empty, TLS 1.3 transport credentials verified against caFile
// are used; otherwise insecure transport is used for cluster-local plugins.
func DialGRPCSSHAuthenticator(target string, timeout time.Duration, caFile string) (*GRPCSSHAuthenticator, *grpc.ClientConn, error) {
	if strings.TrimSpace(target) == "" {
		return nil, nil, errors.New("gRPC SSH auth plugin target is required")
	}

	var creds credentials.TransportCredentials
	if strings.TrimSpace(caFile) != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read gRPC SSH auth CA %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, nil, fmt.Errorf("parse gRPC SSH auth CA %s: no certificates found", caFile)
		}
		creds = credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    pool,
		})
	} else {
		creds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("dial gRPC SSH auth plugin %s: %w", target, err)
	}
	client := sshv1alpha1.NewSSHAuthServiceClient(conn)
	return NewGRPCSSHAuthenticator(client, timeout), conn, nil
}

// Authenticate calls SSHAuthService.Authenticate on the configured gRPC plugin.
// Any RPC error, denied response, or invalid Kubernetes label fails closed.
func (a *GRPCSSHAuthenticator) Authenticate(ctx context.Context, req SSHAuthRequest) (SSHAuthResult, error) {
	if a.client == nil {
		return SSHAuthResult{}, datadssh.ErrUnauthorized
	}

	pbReq := &sshv1alpha1.AuthenticateRequest{
		Username:   req.Username,
		Authority:  req.Authority,
		RemoteAddr: req.RemoteAddr,
		Method:     req.Method,
	}
	switch req.Method {
	case sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY:
		if err := datadssh.ValidatePublicKey(req.PublicKey); err != nil {
			return SSHAuthResult{}, datadssh.ErrUnauthorized
		}
		pbReq.PublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(req.PublicKey)))
		pbReq.PublicKeyFingerprint = ssh.FingerprintSHA256(req.PublicKey)
	case sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PASSWORD:
		pbReq.Password = string(req.Password)
	default:
		return SSHAuthResult{}, datadssh.ErrUnauthorized
	}

	rpcCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	resp, err := a.client.Authenticate(rpcCtx, pbReq)
	if err != nil || resp == nil || !resp.GetAllowed() {
		return SSHAuthResult{}, datadssh.ErrUnauthorized
	}

	ns := resp.GetNamespace()
	ws := resp.GetWorkspace()
	if !dns1123LabelRegexp.MatchString(ns) || !dns1123LabelRegexp.MatchString(ws) {
		return SSHAuthResult{}, datadssh.ErrUnauthorized
	}
	return SSHAuthResult{
		Namespace: ns,
		Workspace: ws,
		UserID:    resp.GetUserId(),
	}, nil
}
