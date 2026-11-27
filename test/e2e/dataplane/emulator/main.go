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

// Command emulator starts a self-contained local Workspace Data Plane stack
// (rckube.Supervisor + mTLS execd/http+ws + mTLS envd/connectrpc + Data Plane
// Gateway) for SDK conformance and E2E testing without a Kubernetes cluster.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nekomeowww/rc/internal/datad/gateway"
	"github.com/nekomeowww/rc/internal/datad/pki"
	envdconnectrpc "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/envd/connectrpc"
	execdhttp "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/execd/http"
	"github.com/nekomeowww/rc/internal/rckube"
)

type staticResolver struct {
	target gateway.Target
}

func (r staticResolver) Resolve(_ context.Context, namespace, name string) (gateway.Target, error) {
	if namespace == r.target.Namespace && name == r.target.Name {
		return r.target, nil
	}
	return gateway.Target{}, gateway.ErrNotFound
}

type readyInfo struct {
	GatewayURL   string `json:"gatewayUrl"`
	ExecdURL     string `json:"execdUrl"`
	EnvdURL      string `json:"envdUrl"`
	Token        string `json:"token"`
	HomeDir      string `json:"homeDir"`
	WorkspaceDir string `json:"workspaceDir"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "dataplane emulator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tmpDir, err := os.MkdirTemp("", "rc-dataplane-e2e-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	homeDir := filepath.Join(tmpDir, "home")
	workspaceDir := filepath.Join(tmpDir, "workspace")
	stateDir := filepath.Join(tmpDir, "state")
	runDir := filepath.Join(tmpDir, "run")
	certDir := filepath.Join(tmpDir, "certs")
	for _, dir := range []string{homeDir, workspaceDir, stateDir, runDir, certDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	ca, err := pki.GenerateCA(time.Hour)
	if err != nil {
		return err
	}
	namespace, name := "demo", "sandbox"
	serverName := pki.WorkspaceServerName(namespace, name)
	srvCert, err := pki.IssueServerCert(ca.CertPEM, ca.KeyPEM, serverName, time.Hour)
	if err != nil {
		return err
	}
	cliCert, err := pki.IssueClientCert(ca.CertPEM, ca.KeyPEM, "", time.Hour)
	if err != nil {
		return err
	}

	caFile := filepath.Join(certDir, "ca.crt")
	certFile := filepath.Join(certDir, "tls.crt")
	keyFile := filepath.Join(certDir, "tls.key")
	if err := os.WriteFile(caFile, ca.CertPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, srvCert.CertPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, srvCert.KeyPEM, 0o600); err != nil {
		return err
	}

	rawToken := make([]byte, 32)
	if _, err := rand.Read(rawToken); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(rawToken)

	supervisor := rckube.NewSupervisor(stateDir, 2*time.Second, rckube.WithRoots(homeDir, workspaceDir, runDir))
	defer supervisor.Shutdown()

	execdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	envdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	gwLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}

	execdSrv := execdhttp.NewServer(supervisor, homeDir, workspaceDir, "")
	envdSrv := envdconnectrpc.NewServer(supervisor, homeDir, "")

	go func() {
		_ = execdSrv.ServeListener(ctx, execdLn, execdhttp.Config{
			CAFile:       caFile,
			CertFile:     certFile,
			KeyFile:      keyFile,
			HomeDir:      homeDir,
			WorkspaceDir: workspaceDir,
		})
	}()
	go func() {
		_ = envdSrv.ServeListener(ctx, envdLn, envdconnectrpc.Config{
			CAFile:   caFile,
			CertFile: certFile,
			KeyFile:  keyFile,
			HomeDir:  homeDir,
		})
	}()

	gw := gateway.NewServer(
		staticResolver{
			target: gateway.Target{
				Namespace:    namespace,
				Name:         name,
				Ready:        true,
				State:        "Running",
				ExecdAddress: execdLn.Addr().String(),
				Address:      envdLn.Addr().String(),
				ServerName:   serverName,
				AccessToken:  token,
			},
		},
		func(_ context.Context) ([]byte, []byte, []byte, error) {
			return ca.CertPEM, cliCert.CertPEM, cliCert.KeyPEM, nil
		},
	)
	gwServer := &http.Server{
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		_ = gwServer.Serve(gwLn)
	}()

	gwBase := "http://" + gwLn.Addr().String()
	info := readyInfo{
		GatewayURL:   gwBase,
		ExecdURL:     fmt.Sprintf("%s/namespaces/%s/workspaces/%s/execd", gwBase, namespace, name),
		EnvdURL:      fmt.Sprintf("%s/namespaces/%s/workspaces/%s/envd", gwBase, namespace, name),
		Token:        token,
		HomeDir:      homeDir,
		WorkspaceDir: workspaceDir,
	}
	if err := json.NewEncoder(os.Stdout).Encode(info); err != nil {
		return err
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = gwServer.Shutdown(shutdownCtx)
	return nil
}
