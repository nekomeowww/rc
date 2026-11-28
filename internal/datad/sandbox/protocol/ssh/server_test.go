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

package datadssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/nekomeowww/rc/internal/datad/pki"
	"github.com/nekomeowww/rc/internal/rckube"
)

func TestAuthorizedKeysParsingAndMatching(t *testing.T) {
	pub1, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub1, err := ssh.NewPublicKey(pub1)
	require.NoError(t, err)

	pub2, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub2, err := ssh.NewPublicKey(pub2)
	require.NoError(t, err)

	pub3, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sshPub3, err := ssh.NewPublicKey(pub3)
	require.NoError(t, err)

	entries := []string{
		"# comment line\n\n" + string(ssh.MarshalAuthorizedKey(sshPub1)),
		string(ssh.MarshalAuthorizedKey(sshPub2)),
	}
	allowed, err := ParseAuthorizedKeys(entries)
	require.NoError(t, err)
	require.Len(t, allowed, 2)

	assert.True(t, MatchAuthorizedKey(sshPub1, allowed))
	assert.True(t, MatchAuthorizedKey(sshPub2, allowed))
	assert.False(t, MatchAuthorizedKey(sshPub3, allowed))
	assert.False(t, MatchAuthorizedKey(nil, allowed))
	assert.False(t, MatchAuthorizedKey(sshPub1, nil))

	_, err = ParseAuthorizedKeys([]string{"not-a-valid-key"})
	require.Error(t, err)

	// OpenSSH authorized_keys options must fail closed so restrictions are never silently stripped.
	restricted := `from="10.0.0.0/8",no-port-forwarding ` + string(ssh.MarshalAuthorizedKey(sshPub1))
	_, err = ParseAuthorizedKeys([]string{restricted})
	require.Error(t, err)

	// Weak 1024-bit RSA keys must be rejected.
	weakRSA, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	weakSSHPub, err := ssh.NewPublicKey(&weakRSA.PublicKey)
	require.NoError(t, err)
	_, err = ParseAuthorizedKeys([]string{string(ssh.MarshalAuthorizedKey(weakSSHPub))})
	require.Error(t, err)
	assert.False(t, MatchAuthorizedKey(weakSSHPub, []ssh.PublicKey{weakSSHPub}))

	// Lenient parser skips malformed, restricted, and weak keys while preserving valid Ed25519 keys.
	mixed := []string{
		"not-a-valid-key\n" + restricted + "\n" + string(ssh.MarshalAuthorizedKey(weakSSHPub)) + "\n" + string(ssh.MarshalAuthorizedKey(sshPub2)),
	}
	lenient := ParseAuthorizedKeysLenient(mixed)
	require.Len(t, lenient, 1)
	assert.True(t, MatchAuthorizedKey(sshPub2, lenient))
	assert.False(t, MatchAuthorizedKey(sshPub1, lenient))
}

func TestNormalizeLoopbackHost(t *testing.T) {
	allowed := []struct {
		in   string
		want string
	}{
		{in: "localhost", want: loopbackIPv4},
		{in: "LOCALHOST", want: loopbackIPv4},
		{in: loopbackIPv4, want: loopbackIPv4},
		{in: "127.0.0.42", want: "127.0.0.42"},
		{in: "::1", want: "::1"},
	}
	for _, tc := range allowed {
		got, ok := normalizeLoopbackHost(tc.in)
		assert.True(t, ok, "expected %q to be allowed", tc.in)
		assert.Equal(t, tc.want, got)
	}

	denied := []string{
		"",
		"0.0.0.0",
		"::",
		"169.254.169.254",
		"10.0.0.1",
		"192.168.1.1",
		"kubernetes.default.svc",
		"example.com",
		"127.0.0.1.nip.io",
	}
	for _, host := range denied {
		_, ok := normalizeLoopbackHost(host)
		assert.False(t, ok, "expected %q to be denied", host)
	}
}

func TestIsAllowedEnvVar(t *testing.T) {
	assert.True(t, isAllowedEnvVar("TERM", "xterm-256color"))
	assert.True(t, isAllowedEnvVar("LANG", "en_US.UTF-8"))
	assert.True(t, isAllowedEnvVar("COLORTERM", "truecolor"))
	assert.True(t, isAllowedEnvVar("LC_ALL", "C.UTF-8"))

	assert.False(t, isAllowedEnvVar("LD_PRELOAD", "/tmp/evil.so"))
	assert.False(t, isAllowedEnvVar("LD_LIBRARY_PATH", "/tmp"))
	assert.False(t, isAllowedEnvVar("DYLD_INSERT_LIBRARIES", "/tmp/evil.dylib"))
	assert.False(t, isAllowedEnvVar("PATH", "/tmp/bin"))
	assert.False(t, isAllowedEnvVar("BASH_ENV", "/tmp/evil.sh"))
	assert.False(t, isAllowedEnvVar("TERM", "xterm\x00evil"))
	assert.False(t, isAllowedEnvVar("LANG", "en_US.UTF-8\nEVIL=1"))
	assert.False(t, isAllowedEnvVar("TERM", strings.Repeat("a", maxEnvValueLen+1)))
	assert.False(t, isAllowedEnvVar("LC_evil-name", "C"))
}

func TestServerEnforcesMTLSAndALPN(t *testing.T) {
	tmp := t.TempDir()
	ca, err := pki.GenerateCA(time.Hour)
	require.NoError(t, err)
	serverName := pki.WorkspaceServerName("team-a", "ws-1")
	srvCert, err := pki.IssueServerCert(ca.CertPEM, ca.KeyPEM, serverName, time.Hour)
	require.NoError(t, err)
	cliCert, err := pki.IssueClientCert(ca.CertPEM, ca.KeyPEM, "", time.Hour)
	require.NoError(t, err)

	caFile := filepath.Join(tmp, "ca.crt")
	certFile := filepath.Join(tmp, "tls.crt")
	keyFile := filepath.Join(tmp, "tls.key")
	require.NoError(t, os.WriteFile(caFile, ca.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(certFile, srvCert.CertPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, srvCert.KeyPEM, 0o600))

	supervisor := rckube.NewSupervisor(filepath.Join(tmp, "state"), 2*time.Second, rckube.WithRoots(tmp, tmp, filepath.Join(tmp, "run")))
	t.Cleanup(supervisor.Shutdown)

	srv := NewServer(supervisor, tmp, tmp)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeListener(ctx, ln, Config{
			CAFile:           caFile,
			CertFile:         certFile,
			KeyFile:          keyFile,
			HomeDir:          tmp,
			WorkspaceDir:     tmp,
			HandshakeTimeout: 2 * time.Second,
		})
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-errCh)
	})

	const testUser = "test"

	// 1. Plaintext TCP (no TLS) must be rejected.
	plainConn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	require.NoError(t, err)
	_, _, _, err = ssh.NewClientConn(plainConn, ln.Addr().String(), &ssh.ClientConfig{
		User:            testUser,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test
		Timeout:         2 * time.Second,
	})
	require.Error(t, err, "plaintext SSH without mTLS must be rejected")
	_ = plainConn.Close()

	// 2. TLS with wrong ALPN ("h2") must fail ALPN negotiation or be closed.
	wrongALPNCfg, err := pki.ClientTLSConfigWithALPN(ca.CertPEM, cliCert.CertPEM, cliCert.KeyPEM, serverName, []string{"h2"})
	require.NoError(t, err)
	wrongALPNConn, err := tls.Dial("tcp", ln.Addr().String(), wrongALPNCfg)
	if err == nil {
		_, _, _, sshErr := ssh.NewClientConn(wrongALPNConn, ln.Addr().String(), &ssh.ClientConfig{
			User:            testUser,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test
			Timeout:         2 * time.Second,
		})
		require.Error(t, sshErr, "mTLS with non-SSH ALPN must be rejected")
		_ = wrongALPNConn.Close()
	}

	// 3. TLS without client cert must fail handshake.
	noClientCertCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		NextProtos: []string{pki.SSHProtocolALPN},
		// Trust the CA, but present no client certificate.
		InsecureSkipVerify: true, //nolint:gosec // test verifies server rejects missing client cert
	}
	noCertConn, err := tls.Dial("tcp", ln.Addr().String(), noClientCertCfg)
	if err == nil {
		// TLS 1.3 delivers client-cert rejection on first read.
		buf := make([]byte, 16)
		_ = noCertConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, readErr := noCertConn.Read(buf)
		require.Error(t, readErr, "TLS without client cert must be rejected")
		_ = noCertConn.Close()
	}

	// 4. CA-signed client cert with wrong CommonName must be rejected.
	wrongCNCert, err := pki.IssueClientCert(ca.CertPEM, ca.KeyPEM, "rogue-client.rc.ayaka.io", time.Hour)
	require.NoError(t, err)
	wrongCNCfg, err := pki.ClientTLSConfigWithALPN(ca.CertPEM, wrongCNCert.CertPEM, wrongCNCert.KeyPEM, serverName, []string{pki.SSHProtocolALPN})
	require.NoError(t, err)
	wrongCNConn, err := tls.Dial("tcp", ln.Addr().String(), wrongCNCfg)
	if err == nil {
		_, _, _, sshErr := ssh.NewClientConn(wrongCNConn, ln.Addr().String(), &ssh.ClientConfig{
			User:            testUser,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test
			Timeout:         2 * time.Second,
		})
		require.Error(t, sshErr, "mTLS client cert with non-gateway CommonName must be rejected")
		_ = wrongCNConn.Close()
	}
}
