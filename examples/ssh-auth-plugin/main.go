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

// Command ssh-auth-plugin is a minimal external gRPC SSHAuthService plugin
// demonstrating how a multi-tenant control plane maps (authority, username,
// public key) to a local-cluster rc Workspace.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"

	sshv1alpha1 "github.com/nekomeowww/rc/apis/sdk/go/datad/ssh/v1alpha1"
)

type routeEntry struct {
	username   string
	authority  string
	namespace  string
	workspace  string
	userID     string
	publicKeys []ssh.PublicKey
}

type pluginServer struct {
	sshv1alpha1.UnimplementedSSHAuthServiceServer
	routes []routeEntry
}

func (s *pluginServer) Authenticate(_ context.Context, req *sshv1alpha1.AuthenticateRequest) (*sshv1alpha1.AuthenticateResponse, error) {
	if req.GetMethod() != sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY {
		return &sshv1alpha1.AuthenticateResponse{
			Allowed:    false,
			DenyReason: "only publickey authentication is enabled",
		}, nil
	}

	candidate, _, options, _, err := ssh.ParseAuthorizedKey([]byte(req.GetPublicKey()))
	if err != nil || len(options) > 0 {
		return &sshv1alpha1.AuthenticateResponse{
			Allowed:    false,
			DenyReason: "invalid public key",
		}, nil
	}

	for _, r := range s.routes {
		if r.username != req.GetUsername() {
			continue
		}
		if r.authority != "" && !strings.EqualFold(r.authority, req.GetAuthority()) {
			continue
		}
		if matchKey(candidate, r.publicKeys) {
			slog.Info("SSH auth allowed",
				"user", req.GetUsername(),
				"authority", req.GetAuthority(),
				"fingerprint", req.GetPublicKeyFingerprint(),
				"namespace", r.namespace,
				"workspace", r.workspace,
			)
			return &sshv1alpha1.AuthenticateResponse{
				Allowed:   true,
				Namespace: r.namespace,
				Workspace: r.workspace,
				UserId:    r.userID,
			}, nil
		}
	}

	slog.Info("SSH auth denied",
		"user", req.GetUsername(),
		"authority", req.GetAuthority(),
		"remoteAddr", req.GetRemoteAddr(),
	)
	return &sshv1alpha1.AuthenticateResponse{
		Allowed:    false,
		DenyReason: "unauthorized",
	}, nil
}

func matchKey(candidate ssh.PublicKey, allowed []ssh.PublicKey) bool {
	candBytes := candidate.Marshal()
	candType := candidate.Type()
	matched := 0
	for _, key := range allowed {
		eq := subtle.ConstantTimeCompare(candBytes, key.Marshal())
		if key.Type() == candType {
			matched |= eq
		}
	}
	return matched == 1
}

func parseKeyFiles(paths string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for p := range strings.SplitSeq(paths, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read public key file %s: %w", p, err)
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			pub, _, options, _, err := ssh.ParseAuthorizedKey([]byte(trimmed))
			if err != nil {
				return nil, fmt.Errorf("parse public key in %s: %w", p, err)
			}
			if len(options) > 0 {
				return nil, fmt.Errorf("unsupported authorized_keys options in %s", p)
			}
			keys = append(keys, pub)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("at least one public key is required via --pubkey-files")
	}
	return keys, nil
}

func main() {
	var listenAddr string
	var username string
	var authority string
	var namespace string
	var workspace string
	var userID string
	var pubKeyFiles string

	flag.StringVar(&listenAddr, "listen", "127.0.0.1:9090", "gRPC listen address")
	flag.StringVar(&username, "username", "alice", "Allowed SSH username")
	flag.StringVar(&authority, "authority", "ws-1.example.com", "Expected TLS SNI authority (empty matches any)")
	flag.StringVar(&namespace, "namespace", "team-a", "Target Kubernetes namespace")
	flag.StringVar(&workspace, "workspace", "sandbox-1", "Target Workspace name")
	flag.StringVar(&userID, "user-id", "user-1", "External user ID for audit metadata")
	flag.StringVar(&pubKeyFiles, "pubkey-files", "", "Comma-separated paths to OpenSSH .pub files")
	flag.Parse()

	keys, err := parseKeyFiles(pubKeyFiles)
	if err != nil {
		slog.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		slog.Error("Failed to listen", "addr", listenAddr, "error", err)
		os.Exit(1)
	}

	srv := grpc.NewServer()
	sshv1alpha1.RegisterSSHAuthServiceServer(srv, &pluginServer{
		routes: []routeEntry{
			{
				username:   username,
				authority:  authority,
				namespace:  namespace,
				workspace:  workspace,
				userID:     userID,
				publicKeys: keys,
			},
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	slog.Info("Starting SSHAuthService gRPC plugin",
		"listen", listenAddr,
		"username", username,
		"authority", authority,
		"target", namespace+"/"+workspace,
		"keys", len(keys),
	)
	if err := srv.Serve(ln); err != nil {
		slog.Error("gRPC server exited with error", "error", err)
		os.Exit(1)
	}
}
