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
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nekomeowww/rc/internal/datad/gateway"
	"github.com/nekomeowww/rc/internal/datad/pki"
	datadssh "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/ssh"
	"github.com/nekomeowww/rc/internal/rckube"
)

const (
	testSSHNamespace    = "team-a"
	testSSHWorkspace    = "ws-1"
	testSSHTargetKey    = "team-a/ws-1"
	testSSHStateRunning = "Running"
	testSSHUserTarget   = "ws-1.team-a"
	testOSWindows       = "windows"
)

func TestParseSSHUsername(t *testing.T) {
	ns, ws, err := gateway.ParseSSHUsername("sandbox-1.team-a")
	require.NoError(t, err)
	assert.Equal(t, "team-a", ns)
	assert.Equal(t, "sandbox-1", ws)

	ns, ws, err = gateway.ParseSSHUsername("alice@sandbox-1.team-a")
	require.NoError(t, err)
	assert.Equal(t, "team-a", ns)
	assert.Equal(t, "sandbox-1", ws)

	invalid := []string{
		"",
		"sandbox-only",
		"a.b.c",
		"../etc.team-a",
		"WS.team-a",
		"-bad.team-a",
		"ws.-bad",
		strings.Repeat("a", 64) + ".team-a",
	}
	for _, raw := range invalid {
		_, _, parseErr := gateway.ParseSSHUsername(raw)
		assert.Error(t, parseErr, "expected %q to fail username parsing", raw)
	}
}

func generateTestEd25519KeyPair(t *testing.T) (ssh.Signer, string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)

	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600))

	return signer, string(ssh.MarshalAuthorizedKey(sshPub)), keyPath
}

func TestSSHGatewayEndToEndWithEd25519AndMTLS(t *testing.T) {
	tmp := t.TempDir()
	homeDir := filepath.Join(tmp, "home")
	workspaceDir := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))

	// Issue internal Data Plane CA, Pod server cert, and Gateway client cert.
	ca, err := pki.GenerateCA(time.Hour)
	require.NoError(t, err)
	serverName := pki.WorkspaceServerName(testSSHNamespace, testSSHWorkspace)
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

	// Generate local Ed25519 client key pairs (one authorized, one unauthorized).
	goodSigner, goodPubKey, goodKeyPath := generateTestEd25519KeyPair(t)
	badSigner, _, _ := generateTestEd25519KeyPair(t)
	gwHostKey, err := datadssh.GenerateEd25519Signer()
	require.NoError(t, err)

	accessToken := "super-secret-workspace-access-token-0123456789"
	resolver := &mapResolver{
		targets: map[string]gateway.Target{
			testSSHTargetKey: {
				Namespace:         testSSHNamespace,
				Name:              testSSHWorkspace,
				Ready:             true,
				State:             testSSHStateRunning,
				SSHAddress:        podLn.Addr().String(),
				SSHEnabled:        true,
				SSHAuthorizedKeys: []string{goodPubKey},
				ServerName:        serverName,
				AccessToken:       accessToken,
			},
			"team-a/ws-suspended": {
				Namespace:         testSSHNamespace,
				Name:              "ws-suspended",
				Ready:             false,
				State:             "Suspended",
				SSHEnabled:        true,
				SSHAuthorizedKeys: []string{goodPubKey},
				AccessToken:       accessToken,
			},
			"team-a/ws-no-ssh": {
				Namespace:   testSSHNamespace,
				Name:        "ws-no-ssh",
				Ready:       true,
				State:       testSSHStateRunning,
				SSHEnabled:  false,
				AccessToken: accessToken,
			},
		},
	}

	certProvider := func(_ context.Context) ([]byte, []byte, []byte, error) {
		return ca.CertPEM, cliCert.CertPEM, cliCert.KeyPEM, nil
	}

	sshGW, err := gateway.NewSSHServer(resolver, certProvider, gateway.SSHConfig{
		HostKey: gwHostKey,
	})
	require.NoError(t, err)

	gwLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gwErrCh := make(chan error, 1)
	go func() {
		gwErrCh <- sshGW.ServeListener(ctx, gwLn)
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-gwErrCh)
		require.NoError(t, <-podErrCh)
	})

	hostKeyCallback := ssh.FixedHostKey(gwHostKey.PublicKey())

	// 1. Unauthorized Ed25519 key must be rejected.
	_, err = ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
		User:            testSSHUserTarget,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(badSigner)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         3 * time.Second,
	})
	require.Error(t, err, "unauthorized Ed25519 key must be rejected")

	// 2. Wrong password must be rejected.
	_, err = ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
		User:            testSSHUserTarget,
		Auth:            []ssh.AuthMethod{ssh.Password("wrong-token")},
		HostKeyCallback: hostKeyCallback,
		Timeout:         3 * time.Second,
	})
	require.Error(t, err, "wrong access token password must be rejected")

	// 3. Workspace with SSH disabled must reject even with valid token.
	_, err = ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
		User:            "ws-no-ssh.team-a",
		Auth:            []ssh.AuthMethod{ssh.Password(accessToken)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         3 * time.Second,
	})
	require.Error(t, err, "workspace with SSH disabled must be rejected")

	// 4. Suspended workspace authenticates but rejects channels (never auto-starts).
	suspClient, err := ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
		User:            "ws-suspended.team-a",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(goodSigner)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         3 * time.Second,
	})
	require.NoError(t, err)
	_, err = suspClient.NewSession()
	require.Error(t, err, "suspended workspace must reject new channels")
	_ = suspClient.Close()

	// 5. Connect with authorized local Ed25519 key (with pinned host key!).
	client, err := ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
		Config:          datadssh.HardenedSSHConfig(),
		User:            "neko@ws-1.team-a",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(goodSigner)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	// 5a. Exec command with stdout, stderr, and exit code.
	sess, err := client.NewSession()
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if runtime.GOOS == testOSWindows {
		require.NoError(t, sess.Run("echo hello-ssh"))
	} else {
		require.NoError(t, sess.Run("echo hello-ssh && echo err-line >&2"))
		assert.Contains(t, stderr.String(), "err-line")
	}
	assert.Contains(t, stdout.String(), "hello-ssh")
	_ = sess.Close()

	// 5b. Non-zero exit code propagation.
	sessExit, err := client.NewSession()
	require.NoError(t, err)
	exitErr := sessExit.Run("exit 7")
	var sshExitErr *ssh.ExitError
	require.ErrorAs(t, exitErr, &sshExitErr)
	assert.Equal(t, 7, sshExitErr.ExitStatus())
	_ = sessExit.Close()

	// 5c. Dangerous env injection (LD_PRELOAD) must be rejected; safe env (LANG) accepted.
	sessEnv, err := client.NewSession()
	require.NoError(t, err)
	assert.Error(t, sessEnv.Setenv("LD_PRELOAD", "/tmp/evil.so"))
	assert.NoError(t, sessEnv.Setenv("LANG", "C.UTF-8"))
	_ = sessEnv.Close()

	// 5d. Stdin + EOF propagation.
	if runtime.GOOS != testOSWindows {
		sessIn, err := client.NewSession()
		require.NoError(t, err)
		sessIn.Stdin = strings.NewReader("piped-input-data\n")
		out, err := sessIn.Output("cat")
		require.NoError(t, err)
		assert.Equal(t, "piped-input-data\n", string(out))
		_ = sessIn.Close()
	}

	// 5e. Interactive PTY shell + resize.
	if runtime.GOOS != testOSWindows {
		sessPTY, err := client.NewSession()
		require.NoError(t, err)
		require.NoError(t, sessPTY.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}))
		stdinPipe, err := sessPTY.StdinPipe()
		require.NoError(t, err)
		var ptyOut bytes.Buffer
		sessPTY.Stdout = &ptyOut
		require.NoError(t, sessPTY.Shell())
		require.NoError(t, sessPTY.WindowChange(40, 120))
		_, err = io.WriteString(stdinPipe, "echo pty-ok && exit 0\n")
		require.NoError(t, err)
		require.NoError(t, sessPTY.Wait())
		assert.Contains(t, ptyOut.String(), "pty-ok")
		_ = sessPTY.Close()
	}

	// 5f. SFTP upload and download.
	sftpClient, err := sftp.NewClient(client)
	require.NoError(t, err)
	remotePath := filepath.Join(workspaceDir, "sftp-hello.txt")
	f, err := sftpClient.Create(remotePath)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello via sftp"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	rf, err := sftpClient.Open(remotePath)
	require.NoError(t, err)
	content, err := io.ReadAll(rf)
	require.NoError(t, err)
	require.NoError(t, rf.Close())
	assert.Equal(t, "hello via sftp", string(content))
	require.NoError(t, sftpClient.Close())

	// 5g. direct-tcpip to loopback inside the Pod succeeds.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = echoLn.Close() })
	go func() {
		conn, acceptErr := echoLn.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()

	fwdConn, err := client.Dial("tcp", echoLn.Addr().String())
	require.NoError(t, err)
	_, err = fwdConn.Write([]byte("ping-loopback"))
	require.NoError(t, err)
	reply := make([]byte, len("ping-loopback"))
	_, err = io.ReadFull(fwdConn, reply)
	require.NoError(t, err)
	assert.Equal(t, "ping-loopback", string(reply))
	_ = fwdConn.Close()

	// 5h. direct-tcpip to non-loopback (cloud metadata SSRF) must be REJECTED.
	_, err = client.Dial("tcp", "169.254.169.254:80")
	require.Error(t, err, "direct-tcpip to non-loopback IP must be prohibited")

	// 5i. Remote port forwarding (tcpip-forward) on the Gateway must be REJECTED.
	_, err = client.Listen("tcp", "127.0.0.1:0")
	require.Error(t, err, "tcpip-forward global request must be rejected")

	// 6. AccessToken password auth also works.
	pwClient, err := ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
		User:            testSSHUserTarget,
		Auth:            []ssh.AuthMethod{ssh.Password(accessToken)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         5 * time.Second,
	})
	require.NoError(t, err)
	pwSess, err := pwClient.NewSession()
	require.NoError(t, err)
	pwOut, err := pwSess.Output("echo password-auth-ok")
	require.NoError(t, err)
	assert.Contains(t, string(pwOut), "password-auth-ok")
	_ = pwSess.Close()
	_ = pwClient.Close()

	// 7. Real OpenSSH CLI (`ssh -i <ed25519-key>`) smoke test when `ssh` is installed.
	if sshBin, lookErr := exec.LookPath("ssh"); lookErr == nil && runtime.GOOS != testOSWindows {
		host, port, splitErr := net.SplitHostPort(gwLn.Addr().String())
		require.NoError(t, splitErr)
		knownHosts := filepath.Join(tmp, "known_hosts")
		knownLine := fmt.Sprintf("[%s]:%s %s", host, port, string(ssh.MarshalAuthorizedKey(gwHostKey.PublicKey())))
		require.NoError(t, os.WriteFile(knownHosts, []byte(knownLine), 0o600))

		cmd := exec.CommandContext(ctx, sshBin,
			"-i", goodKeyPath,
			"-p", port,
			"-o", "IdentitiesOnly=yes",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile="+knownHosts,
			testSSHUserTarget+"@"+host,
			"echo openssh-ed25519-ok && cat sftp-hello.txt",
		)
		out, runErr := cmd.CombinedOutput()
		require.NoError(t, runErr, "openssh cli failed: %s", string(out))
		assert.Contains(t, string(out), "openssh-ed25519-ok")
		assert.Contains(t, string(out), "hello via sftp")
	}

	// 8. Abrupt client disconnect during an active PTY session must NOT deadlock
	// gateway/in-pod cleanup or leak connection slots.
	if runtime.GOOS != testOSWindows {
		abruptClient, err := ssh.Dial("tcp", gwLn.Addr().String(), &ssh.ClientConfig{
			User:            testSSHUserTarget,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(goodSigner)},
			HostKeyCallback: hostKeyCallback,
			Timeout:         5 * time.Second,
		})
		require.NoError(t, err)
		abruptSess, err := abruptClient.NewSession()
		require.NoError(t, err)
		require.NoError(t, abruptSess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}))
		require.NoError(t, abruptSess.Start("sleep 60"))
		// Drop the TCP/SSH connection immediately without exiting the shell.
		require.NoError(t, abruptClient.Close())
	}
}
