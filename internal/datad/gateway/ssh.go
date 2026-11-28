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
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	sshv1alpha1 "github.com/nekomeowww/rc/apis/sdk/go/datad/ssh/v1alpha1"
	"github.com/nekomeowww/rc/internal/datad/pki"
	datadssh "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/ssh"
)

const (
	defaultSSHMaxConnections      = 256
	defaultSSHMaxChannelsPerConn  = 32
	defaultSSHHandshakeTimeout    = 15 * time.Second
	defaultSSHUpstreamDialTimeout = 10 * time.Second
	defaultSSHRejectGraceTimeout  = 3 * time.Second
	defaultSSHMaxAuthTries        = 6
	minSSHPasswordTokenLength     = 32
	sshPermNamespaceKey           = "rc-namespace"
	sshPermWorkspaceKey           = "rc-workspace"
	sshPermAuthMethodKey          = "rc-auth-method"
	sshPermUserIDKey              = "rc-user-id"
)

var dns1123LabelRegexp = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// SSHConfig configures the Data Plane Gateway SSH proxy.
type SSHConfig struct {
	HostKey             ssh.Signer
	Authenticator       SSHAuthenticator
	DefaultAuthority    string
	MaxConnections      int
	MaxChannelsPerConn  int
	HandshakeTimeout    time.Duration
	UpstreamDialTimeout time.Duration
	// DisablePasswordAuth disables SSH password authentication, requiring SSH
	// public keys.
	DisablePasswordAuth bool
}

// SSHServer terminates external SSH connections, authenticates callers against
// Workspace authorized keys/access tokens or an external gRPC SSHAuthenticator
// plugin, and multiplexes SSH channels over TLS 1.3 mTLS directly to the
// Workspace Pod's in-Pod SSH listener.
type SSHServer struct {
	resolver            Resolver
	certProvider        ClientCertProvider
	authenticator       SSHAuthenticator
	defaultAuthority    string
	hostKey             ssh.Signer
	maxConnections      int
	maxChannelsPerConn  int
	handshakeTimeout    time.Duration
	upstreamDialTimeout time.Duration
	disablePasswordAuth bool
}

// NewSSHServer constructs an SSH Data Plane Gateway proxy.
func NewSSHServer(resolver Resolver, certProvider ClientCertProvider, cfg SSHConfig) (*SSHServer, error) {
	if resolver == nil {
		return nil, errors.New("resolver is required")
	}
	hostKey := cfg.HostKey
	if hostKey == nil {
		var err error
		hostKey, err = datadssh.GenerateEd25519Signer()
		if err != nil {
			return nil, err
		}
	}
	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = defaultSSHMaxConnections
	}
	maxChans := cfg.MaxChannelsPerConn
	if maxChans <= 0 {
		maxChans = defaultSSHMaxChannelsPerConn
	}
	handshakeTimeout := cfg.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultSSHHandshakeTimeout
	}
	upstreamDialTimeout := cfg.UpstreamDialTimeout
	if upstreamDialTimeout <= 0 {
		upstreamDialTimeout = defaultSSHUpstreamDialTimeout
	}
	return &SSHServer{
		resolver:            resolver,
		certProvider:        certProvider,
		authenticator:       cfg.Authenticator,
		defaultAuthority:    strings.TrimSpace(cfg.DefaultAuthority),
		hostKey:             hostKey,
		maxConnections:      maxConns,
		maxChannelsPerConn:  maxChans,
		handshakeTimeout:    handshakeTimeout,
		upstreamDialTimeout: upstreamDialTimeout,
		disablePasswordAuth: cfg.DisablePasswordAuth,
	}, nil
}

// ParseSSHUsername parses a Gateway SSH username into a Kubernetes namespace
// and Workspace name. Supported formats:
//   - "<workspace>.<namespace>"
//   - "<user>@<workspace>.<namespace>"
//
// Both workspace and namespace are strictly validated as DNS-1123 labels.
func ParseSSHUsername(raw string) (namespace, workspace string, err error) {
	if raw == "" || len(raw) > 256 {
		return "", "", errors.New("invalid ssh username")
	}
	target := raw
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		target = raw[at+1:]
	}
	parts := strings.Split(target, ".")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid ssh target %q: expected <workspace>.<namespace>", target)
	}
	workspace, namespace = parts[0], parts[1]
	if !dns1123LabelRegexp.MatchString(workspace) || !dns1123LabelRegexp.MatchString(namespace) {
		return "", "", fmt.Errorf("invalid ssh target %q: workspace and namespace must be DNS-1123 labels", target)
	}
	return namespace, workspace, nil
}

// Serve binds addr and serves SSH proxy connections until ctx is cancelled.
func (s *SSHServer) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on ssh gateway %s: %w", addr, err)
	}
	return s.ServeListener(ctx, ln)
}

// ServeListener serves SSH proxy connections on ln until ctx is cancelled.
func (s *SSHServer) ServeListener(ctx context.Context, ln net.Listener) error {
	return s.serveListener(ctx, ln, false)
}

// ServeTLS binds addr and serves TLS-wrapped SSH connections (extracting the
// target subdomain from TLS SNI as authority) until ctx is cancelled.
func (s *SSHServer) ServeTLS(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on ssh tls gateway %s: %w", addr, err)
	}
	return s.ServeTLSListener(ctx, ln, tlsCfg)
}

// ServeTLSListener serves TLS-wrapped SSH connections on ln until ctx is
// cancelled, forwarding each connection's TLS SNI ServerName as authority.
func (s *SSHServer) ServeTLSListener(ctx context.Context, ln net.Listener, tlsCfg *tls.Config) error {
	if tlsCfg == nil {
		_ = ln.Close()
		return errors.New("tls config is required for ServeTLSListener")
	}
	cloned := tlsCfg.Clone()
	if cloned.MinVersion == 0 {
		cloned.MinVersion = tls.VersionTLS13
	}
	return s.serveListener(ctx, tls.NewListener(ln, cloned), true)
}

func (s *SSHServer) serveListener(ctx context.Context, ln net.Listener, extractSNI bool) error {
	sem := make(chan struct{}, s.maxConnections)

	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()
	go func() {
		<-serveCtx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		rawConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}

		select {
		case sem <- struct{}{}:
		default:
			_ = rawConn.Close()
			continue
		}

		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			defer func() { <-sem }()
			s.handleConn(ctx, c, extractSNI)
		}(rawConn)
	}
}

type authResolveCache struct {
	mu      sync.Mutex
	key     string
	target  Target
	err     error
	fetched bool
}

func (s *SSHServer) resolveForAuth(ctx context.Context, cache *authResolveCache, ns, name string) (Target, error) {
	k := ns + "/" + name
	cache.mu.Lock()
	if cache.fetched && cache.key == k {
		t, err := cache.target, cache.err
		cache.mu.Unlock()
		return t, err
	}
	cache.mu.Unlock()

	authCtx, cancel := context.WithTimeout(ctx, s.handshakeTimeout)
	defer cancel()
	t, err := s.resolver.Resolve(authCtx, ns, name)

	cache.mu.Lock()
	cache.key = k
	cache.target = t
	cache.err = err
	cache.fetched = true
	cache.mu.Unlock()
	return t, err
}

func (s *SSHServer) serverConfig(ctx context.Context, cache *authResolveCache, authority string) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		Config:            datadssh.HardenedSSHConfig(),
		MaxAuthTries:      defaultSSHMaxAuthTries,
		ServerVersion:     "SSH-2.0-rc-gateway",
		PublicKeyCallback: s.publicKeyCallback(ctx, cache, authority),
	}
	if !s.disablePasswordAuth {
		cfg.PasswordCallback = s.passwordCallback(ctx, cache, authority)
	}
	datadssh.RestrictServerConfigPublicKeyAlgorithms(cfg)
	cfg.AddHostKey(s.hostKey)
	return cfg
}

func (s *SSHServer) publicKeyCallback(ctx context.Context, cache *authResolveCache, authority string) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if s.authenticator != nil {
			res, err := s.authenticator.Authenticate(ctx, SSHAuthRequest{
				Username:   conn.User(),
				Authority:  authority,
				RemoteAddr: conn.RemoteAddr().String(),
				Method:     sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PUBLIC_KEY,
				PublicKey:  key,
			})
			if err != nil {
				return nil, datadssh.ErrUnauthorized
			}
			return &ssh.Permissions{
				Extensions: map[string]string{
					sshPermNamespaceKey:  res.Namespace,
					sshPermWorkspaceKey:  res.Workspace,
					sshPermAuthMethodKey: "publickey",
					sshPermUserIDKey:     res.UserID,
				},
			}, nil
		}

		ns, name, err := ParseSSHUsername(conn.User())
		if err != nil {
			return nil, datadssh.ErrUnauthorized
		}
		target, err := s.resolveForAuth(ctx, cache, ns, name)
		if err != nil || !target.SSHEnabled || len(target.SSHAuthorizedKeys) == 0 {
			return nil, datadssh.ErrUnauthorized
		}
		allowed := datadssh.ParseAuthorizedKeysLenient(target.SSHAuthorizedKeys)
		if !datadssh.MatchAuthorizedKey(key, allowed) {
			return nil, datadssh.ErrUnauthorized
		}
		return &ssh.Permissions{
			Extensions: map[string]string{
				sshPermNamespaceKey:  ns,
				sshPermWorkspaceKey:  name,
				sshPermAuthMethodKey: "publickey",
			},
		}, nil
	}
}

func (s *SSHServer) passwordCallback(ctx context.Context, cache *authResolveCache, authority string) func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if s.authenticator != nil {
			res, err := s.authenticator.Authenticate(ctx, SSHAuthRequest{
				Username:   conn.User(),
				Authority:  authority,
				RemoteAddr: conn.RemoteAddr().String(),
				Method:     sshv1alpha1.SSHAuthMethod_SSH_AUTH_METHOD_PASSWORD,
				Password:   password,
			})
			if err != nil {
				return nil, datadssh.ErrUnauthorized
			}
			return &ssh.Permissions{
				Extensions: map[string]string{
					sshPermNamespaceKey:  res.Namespace,
					sshPermWorkspaceKey:  res.Workspace,
					sshPermAuthMethodKey: "password",
					sshPermUserIDKey:     res.UserID,
				},
			}, nil
		}

		ns, name, err := ParseSSHUsername(conn.User())
		if err != nil {
			return nil, datadssh.ErrUnauthorized
		}
		target, err := s.resolveForAuth(ctx, cache, ns, name)
		if err != nil || !target.SSHEnabled || len(target.AccessToken) < minSSHPasswordTokenLength {
			return nil, datadssh.ErrUnauthorized
		}
		if !constantTimePasswordMatch(password, target.AccessToken) {
			return nil, datadssh.ErrUnauthorized
		}
		return &ssh.Permissions{
			Extensions: map[string]string{
				sshPermNamespaceKey:  ns,
				sshPermWorkspaceKey:  name,
				sshPermAuthMethodKey: "password",
			},
		}, nil
	}
}

// constantTimePasswordMatch hashes both inputs with SHA-256 before comparing
// with subtle.ConstantTimeCompare so variable-length tokens do not leak length.
func constantTimePasswordMatch(provided []byte, expected string) bool {
	providedHash := sha256.Sum256(provided)
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

func (s *SSHServer) handleConn(ctx context.Context, rawConn net.Conn, extractSNI bool) {
	defer func() { _ = rawConn.Close() }()

	if err := rawConn.SetDeadline(time.Now().Add(s.handshakeTimeout)); err != nil {
		return
	}

	authority := s.defaultAuthority
	if extractSNI {
		tlsConn, ok := rawConn.(*tls.Conn)
		if !ok {
			return
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return
		}
		if sni := strings.TrimSpace(tlsConn.ConnectionState().ServerName); sni != "" {
			authority = sni
		}
	}

	cache := &authResolveCache{}
	serverConn, chans, reqs, err := ssh.NewServerConn(rawConn, s.serverConfig(ctx, cache, authority))
	if err != nil {
		return
	}
	defer func() { _ = serverConn.Close() }()

	_ = rawConn.SetDeadline(time.Time{})

	if serverConn.Permissions == nil {
		return
	}
	ns := serverConn.Permissions.Extensions[sshPermNamespaceKey]
	name := serverConn.Permissions.Extensions[sshPermWorkspaceKey]
	if ns == "" || name == "" {
		return
	}

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	go func() {
		<-connCtx.Done()
		_ = serverConn.Close()
	}()

	target, err := s.resolveForAuth(connCtx, cache, ns, name)
	if err != nil || !target.SSHEnabled || !target.Ready || target.SSHAddress == "" {
		rejectAndClose(rawConn, serverConn, chans, reqs, "workspace is not ready for ssh")
		return
	}

	upstreamClient, err := s.dialUpstream(connCtx, target)
	if err != nil {
		rejectAndClose(rawConn, serverConn, chans, reqs, "failed to connect to workspace ssh runtime")
		return
	}
	defer func() { _ = upstreamClient.Close() }()

	// Tear down the downstream client if the upstream Pod connection dies.
	go func() {
		_ = upstreamClient.Wait()
		connCancel()
	}()

	go handleGatewayGlobalRequests(reqs)

	chanSem := make(chan struct{}, s.maxChannelsPerConn)
	var chanWG sync.WaitGroup

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session", "direct-tcpip":
			select {
			case chanSem <- struct{}{}:
			default:
				_ = newChan.Reject(ssh.ResourceShortage, "too many concurrent channels")
				continue
			}
			chanWG.Add(1)
			go func(nc ssh.NewChannel) {
				defer chanWG.Done()
				defer func() { <-chanSem }()
				proxyChannel(nc, upstreamClient)
			}(newChan)
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, fmt.Sprintf("unsupported channel type %q", newChan.ChannelType()))
		}
	}

	// Close upstream BEFORE waiting for proxyChannel goroutines so abrupt
	// downstream disconnects immediately release upstream channels and slots.
	_ = upstreamClient.Close()
	connCancel()
	chanWG.Wait()
}

func (s *SSHServer) dialUpstream(ctx context.Context, target Target) (*ssh.Client, error) {
	dialCtx, cancel := context.WithTimeout(ctx, s.upstreamDialTimeout)
	defer cancel()

	var (
		rawConn net.Conn
		err     error
	)
	if target.Insecure {
		dialer := &net.Dialer{Timeout: s.upstreamDialTimeout}
		rawConn, err = dialer.DialContext(dialCtx, "tcp", target.SSHAddress)
		if err != nil {
			return nil, fmt.Errorf("dial insecure ssh upstream: %w", err)
		}
	} else {
		if s.certProvider == nil {
			return nil, errors.New("mTLS client certificate provider is not configured")
		}
		caPEM, certPEM, keyPEM, certErr := s.certProvider(dialCtx)
		if certErr != nil {
			return nil, fmt.Errorf("load gateway client cert: %w", certErr)
		}
		serverName := target.ServerName
		if serverName == "" {
			serverName = pki.WorkspaceServerName(target.Namespace, target.Name)
		}
		tlsCfg, tlsErr := pki.ClientTLSConfigWithALPN(caPEM, certPEM, keyPEM, serverName, []string{pki.SSHProtocolALPN})
		if tlsErr != nil {
			return nil, fmt.Errorf("build upstream mTLS config: %w", tlsErr)
		}
		tlsDialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: s.upstreamDialTimeout},
			Config:    tlsCfg,
		}
		rawConn, err = tlsDialer.DialContext(dialCtx, "tcp", target.SSHAddress)
		if err != nil {
			return nil, fmt.Errorf("dial mTLS ssh upstream: %w", err)
		}
		if tlsConn, ok := rawConn.(*tls.Conn); ok {
			if tlsConn.ConnectionState().NegotiatedProtocol != pki.SSHProtocolALPN {
				_ = rawConn.Close()
				return nil, fmt.Errorf("unexpected upstream ALPN %q", tlsConn.ConnectionState().NegotiatedProtocol)
			}
		}
	}

	if err := rawConn.SetDeadline(time.Now().Add(s.upstreamDialTimeout)); err != nil {
		_ = rawConn.Close()
		return nil, err
	}

	clientCfg := &ssh.ClientConfig{
		Config: datadssh.HardenedSSHConfig(),
		User:   target.Name,
		// The outer transport is TLS 1.3 mTLS authenticated against the Data Plane
		// CA and pinned to <name>.<namespace>.workspace.rc.ayaka.io, so the
		// ephemeral in-Pod SSH host key does not carry the trust root.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // Transport identity is enforced by TLS 1.3 mTLS.
		Timeout:         s.upstreamDialTimeout,
	}
	c, chans, reqs, err := ssh.NewClientConn(rawConn, target.SSHAddress, clientCfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("ssh handshake with upstream: %w", err)
	}
	_ = rawConn.SetDeadline(time.Time{})

	return ssh.NewClient(c, chans, reqs), nil
}

func handleGatewayGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "keepalive@openssh.com":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			// Never allow tcpip-forward on the Gateway itself.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// rejectAndClose rejects any immediate channel request with reason and closes
// the connection after a short bounded grace period so clients targeting a
// suspended Workspace cannot hold Gateway connection slots indefinitely.
func rejectAndClose(rawConn net.Conn, serverConn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request, reason string) {
	go ssh.DiscardRequests(reqs)
	_ = rawConn.SetDeadline(time.Now().Add(defaultSSHRejectGraceTimeout))
	timer := time.AfterFunc(defaultSSHRejectGraceTimeout, func() {
		_ = serverConn.Close()
	})
	defer timer.Stop()

	for nc := range chans {
		_ = nc.Reject(ssh.ConnectionFailed, reason)
		_ = serverConn.Close()
		return
	}
}

func proxyChannel(newChan ssh.NewChannel, upstream *ssh.Client) {
	upChan, upReqs, err := upstream.OpenChannel(newChan.ChannelType(), newChan.ExtraData())
	if err != nil {
		if openErr, ok := errors.AsType[*ssh.OpenChannelError](err); ok {
			_ = newChan.Reject(openErr.Reason, openErr.Message)
		} else {
			_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}

	downChan, downReqs, err := newChan.Accept()
	if err != nil {
		_ = upChan.Close()
		return
	}

	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = downChan.Close()
			_ = upChan.Close()
		})
	}
	defer closeBoth()

	// Forward downstream requests -> upstream channel. When downReqs closes
	// (SSH_MSG_CHANNEL_CLOSE or connection drop), tear down both sides so
	// upstream channels do not linger on multiplexed connections.
	var downReqWG sync.WaitGroup
	downReqWG.Go(func() {
		for req := range downReqs {
			ok, sendErr := upChan.SendRequest(req.Type, req.WantReply, req.Payload)
			if req.WantReply {
				_ = req.Reply(sendErr == nil && ok, nil)
			}
		}
		closeBoth()
	})

	// Forward downstream stdin -> upstream stdin; half-close upstream on EOF.
	var downInWG sync.WaitGroup
	downInWG.Go(func() {
		_, _ = io.Copy(upChan, downChan)
		_ = upChan.CloseWrite()
	})

	// Forward upstream stdout and stderr; half-close downstream immediately
	// once both streams finish (required for direct-tcpip half-close and to
	// guarantee all output precedes exit-status).
	upOutDone := make(chan struct{})
	go func() {
		defer close(upOutDone)
		var outWG sync.WaitGroup
		outWG.Add(2)
		go func() {
			defer outWG.Done()
			_, _ = io.Copy(downChan, upChan)
		}()
		go func() {
			defer outWG.Done()
			_, _ = io.Copy(downChan.Stderr(), upChan.Stderr())
		}()
		outWG.Wait()
		_ = downChan.CloseWrite()
	}()

	// Forward upstream requests -> downstream channel, deferring terminal
	// exit-status / exit-signal until stdout and stderr have drained.
	var deferred []*ssh.Request
	for req := range upReqs {
		if req.Type == "exit-status" || req.Type == "exit-signal" {
			deferred = append(deferred, req)
			continue
		}
		ok, sendErr := downChan.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			_ = req.Reply(sendErr == nil && ok, nil)
		}
	}

	<-upOutDone
	for _, req := range deferred {
		ok, sendErr := downChan.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			_ = req.Reply(sendErr == nil && ok, nil)
		}
	}

	closeBoth()
	downInWG.Wait()
	downReqWG.Wait()
}
