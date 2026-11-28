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
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"

	sshv1alpha1 "github.com/nekomeowww/rc/apis/sdk/go/datad/ssh/v1alpha1"
	"github.com/nekomeowww/rc/internal/datad/gateway"
	"github.com/nekomeowww/rc/internal/datad/pki"
	datadssh "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/ssh"
	"github.com/nekomeowww/rc/internal/rckube"
)

type fakeSSHAuthPlugin struct {
	sshv1alpha1.UnimplementedSSHAuthServiceServer
	allowedUser      string
	allowedAuthority string
	allowedPubKey    string
	targetNamespace  string
	targetWorkspace  string
}

func (f *fakeSSHAuthPlugin) Authenticate(_ context.Context, req *sshv1alpha1.AuthenticateRequest) (*sshv1alpha1.AuthenticateResponse, error) {
	if req.GetUsername() == "bad-plugin-ns" {
		return &sshv1alpha1.AuthenticateResponse{
			Allowed:   true,
			Namespace: "../invalid",
			Workspace: testSSHWorkspace,
		}, nil
	}
	if req.GetMethod() != sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY {
		return &sshv1alpha1.AuthenticateResponse{Allowed: false, DenyReason: "pubkey required"}, nil
	}
	if req.GetUsername() != f.allowedUser || req.GetAuthority() != f.allowedAuthority {
		return &sshv1alpha1.AuthenticateResponse{Allowed: false, DenyReason: "unknown user or host"}, nil
	}
	if strings.TrimSpace(req.GetPublicKey()) != strings.TrimSpace(f.allowedPubKey) || !strings.HasPrefix(req.GetPublicKeyFingerprint(), "SHA256:") {
		return &sshv1alpha1.AuthenticateResponse{Allowed: false, DenyReason: "key mismatch"}, nil
	}
	return &sshv1alpha1.AuthenticateResponse{
		Allowed:   true,
		Namespace: f.targetNamespace,
		Workspace: f.targetWorkspace,
		UserId:    "uid-neko",
	}, nil
}

func TestSSHGatewayWithGRPCPluginAndTLSSNI(t *testing.T) {
	const testAuthority = "ws-abc.lobe.sh"

	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	workspaceDir := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	// Internal Data Plane CA & Pod mTLS certs.
	ca, err := pki.GenerateCA(time.Hour)
	require.NoError(t, err)
	serverName := pki.WorkspaceServerName(testSSHNamespace, testSSHWorkspace)
	podCert, err := pki.IssueServerCert(ca.CertPEM, ca.KeyPEM, serverName, time.Hour)
	require.NoError(t, err)
	cliCert, err := pki.IssueClientCert(ca.CertPEM, ca.KeyPEM, "", time.Hour)
	require.NoError(t, err)
	// Edge TLS cert for ws-abc.lobe.sh.
	edgeCert, err := pki.IssueServerCert(ca.CertPEM, ca.KeyPEM, testAuthority, time.Hour)
	require.NoError(t, err)
	edgeTLSKeyPair, err := tls.X509KeyPair(edgeCert.CertPEM, edgeCert.KeyPEM)
	require.NoError(t, err)

	certDir := filepath.Join(tmp, "certs")
	require.NoError(t, os.MkdirAll(certDir, 0o700))
	caFile := filepath.Join(certDir, "ca.crt")
	certFile := filepath.Join(certDir, "tls.crt")
	keyFile := filepath.Join(certDir, "tls.key")
	require.NoError(t, os.WriteFile(caFile, ca.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(certFile, podCert.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, podCert.KeyPEM, 0o600))

	supervisor := rckube.NewSupervisor(
		filepath.Join(tmp, "state"),
		2*time.Second,
		rckube.WithRoots(homeDir, workspaceDir, filepath.Join(tmp, "run")),
	)
	t.Cleanup(supervisor.Shutdown)

	// Start in-Pod mTLS SSH server.
	podSSH := datadssh.NewServer(supervisor, homeDir, workspaceDir)
	podLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	podErrCh := make(chan error, 1)
	go func() {
		podErrCh <- podSSH.ServeListener(ctx, podLn, datadssh.Config{
			CAFile:       caFile,
			CertFile:     certFile,
			KeyFile:      keyFile,
			HomeDir:      homeDir,
			WorkspaceDir: workspaceDir,
		})
	}()

	// Start external gRPC SSHAuthService plugin (simulating lobe-iaas).
	goodSigner, goodPubKey, _ := generateTestEd25519KeyPair(t)
	badSigner, _, _ := generateTestEd25519KeyPair(t)

	grpcLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer()
	sshv1alpha1.RegisterSSHAuthServiceServer(grpcServer, &fakeSSHAuthPlugin{
		allowedUser:      "neko",
		allowedAuthority: testAuthority,
		allowedPubKey:    goodPubKey,
		targetNamespace:  testSSHNamespace,
		targetWorkspace:  testSSHWorkspace,
	})
	go func() {
		_ = grpcServer.Serve(grpcLn)
	}()
	t.Cleanup(grpcServer.GracefulStop)

	grpcAuth, grpcConn, err := gateway.DialGRPCSSHAuthenticator(grpcLn.Addr().String(), 3*time.Second, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = grpcConn.Close() })

	gwHostKey, err := datadssh.GenerateEd25519Signer()
	require.NoError(t, err)
	resolver := &mapResolver{
		targets: map[string]gateway.Target{
			testSSHTargetKey: {
				Namespace:  testSSHNamespace,
				Name:       testSSHWorkspace,
				Ready:      true,
				State:      testSSHStateRunning,
				SSHAddress: podLn.Addr().String(),
				SSHEnabled: true,
				ServerName: serverName,
			},
		},
	}
	certProvider := func(_ context.Context) ([]byte, []byte, []byte, error) {
		return ca.CertPEM, cliCert.CertPEM, cliCert.KeyPEM, nil
	}

	sshGW, err := gateway.NewSSHServer(resolver, certProvider, gateway.SSHConfig{
		HostKey:       gwHostKey,
		Authenticator: grpcAuth,
	})
	require.NoError(t, err)

	gwLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gwErrCh := make(chan error, 1)
	go func() {
		gwErrCh <- sshGW.ServeTLSListener(ctx, gwLn, &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{edgeTLSKeyPair},
			NextProtos:   []string{"ssh"},
		})
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-gwErrCh)
		require.NoError(t, <-podErrCh)
	})

	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(ca.CertPEM))

	dialViaSNI := func(sni, user string, signer ssh.Signer) (*ssh.Client, error) {
		tlsConn, dialErr := tls.Dial("tcp", gwLn.Addr().String(), &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    caPool,
			ServerName: sni,
			NextProtos: []string{"ssh"},
		})
		if dialErr != nil {
			return nil, dialErr
		}
		c, chans, reqs, sshErr := ssh.NewClientConn(tlsConn, sni, &ssh.ClientConfig{
			Config:          datadssh.HardenedSSHConfig(),
			User:            user,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.FixedHostKey(gwHostKey.PublicKey()),
			Timeout:         5 * time.Second,
		})
		if sshErr != nil {
			_ = tlsConn.Close()
			return nil, sshErr
		}
		return ssh.NewClient(c, chans, reqs), nil
	}

	// 1. Valid user ("neko") + valid SNI ("ws-abc.lobe.sh") + valid Ed25519 key succeeds.
	client, err := dialViaSNI(testAuthority, "neko", goodSigner)
	require.NoError(t, err)
	sess, err := client.NewSession()
	require.NoError(t, err)
	out, err := sess.Output("echo grpc-sni-ok")
	require.NoError(t, err)
	assert.Contains(t, string(out), "grpc-sni-ok")
	_ = sess.Close()
	_ = client.Close()

	// 2. Wrong key is rejected by gRPC plugin.
	_, err = dialViaSNI(testAuthority, "neko", badSigner)
	require.Error(t, err)

	// 3. Wrong username is rejected by gRPC plugin.
	_, err = dialViaSNI(testAuthority, "other-user", goodSigner)
	require.Error(t, err)

	// 4. Buggy/malicious plugin returning invalid Kubernetes namespace fails closed.
	_, err = dialViaSNI(testAuthority, "bad-plugin-ns", goodSigner)
	require.Error(t, err)
}
