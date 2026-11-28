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
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/nekomeowww/rc/internal/datad/pki"
	"github.com/nekomeowww/rc/internal/rckube"
)

const (
	defaultMaxConnections     = 128
	defaultMaxChannelsPerConn = 32
	defaultHandshakeTimeout   = 15 * time.Second
	defaultDialTimeout        = 5 * time.Second
	maxPTYDimension           = 1000
	maxEnvEntries             = 32
	maxEnvValueLen            = 1024
	envTerm                   = "TERM"
	loopbackIPv4              = "127.0.0.1"
	osWindows                 = "windows"
)

// Config configures the in-Pod mTLS SSH Data Plane server.
type Config struct {
	ListenAddr string
	CAFile     string
	CertFile   string
	KeyFile    string
	// Insecure disables mTLS. Intended only for local tests or already-authenticated transports.
	Insecure           bool
	HomeDir            string
	WorkspaceDir       string
	MaxConnections     int
	MaxChannelsPerConn int
	HandshakeTimeout   time.Duration
}

// Server serves SSH (shell, exec, PTY, SFTP, loopback direct-tcpip) inside
// rc-kube over TLS 1.3 mTLS, backed by rckube.Supervisor.
type Server struct {
	supervisor   *rckube.Supervisor
	homeDir      string
	workspaceDir string
}

// NewServer creates an in-Pod SSH Data Plane server bound to supervisor.
func NewServer(supervisor *rckube.Supervisor, homeDir, workspaceDir string) *Server {
	if homeDir == "" && supervisor != nil {
		homeDir = supervisor.HomeDir()
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	if workspaceDir == "" && supervisor != nil {
		workspaceDir = supervisor.WorkspaceDir()
	}
	if workspaceDir == "" {
		workspaceDir = homeDir
	}
	return &Server{
		supervisor:   supervisor,
		homeDir:      homeDir,
		workspaceDir: workspaceDir,
	}
}

// Serve binds cfg.ListenAddr and serves SSH connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", DefaultPort)
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on ssh address %s: %w", cfg.ListenAddr, err)
	}
	return s.ServeListener(ctx, listener, cfg)
}

// ServeListener serves SSH connections on an already-bound listener until ctx is cancelled.
func (s *Server) ServeListener(ctx context.Context, listener net.Listener, cfg Config) error {
	hostSigner, err := GenerateEd25519Signer()
	if err != nil {
		_ = listener.Close()
		return err
	}

	requireMTLS := !cfg.Insecure
	if requireMTLS {
		if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
			_ = listener.Close()
			return errors.New("ssh data plane mTLS requires ca-file, cert-file, and key-file")
		}
		tlsCfg, err := pki.ReloadingServerTLSConfigWithALPN(
			ctx,
			cfg.CAFile,
			cfg.CertFile,
			cfg.KeyFile,
			[]string{pki.SSHProtocolALPN},
		)
		if err != nil {
			_ = listener.Close()
			return err
		}
		listener = tls.NewListener(listener, tlsCfg)
	}

	sshCfg := &ssh.ServerConfig{
		Config: HardenedSSHConfig(),
		// Inner SSH connections arrive exclusively over the TLS 1.3 mTLS
		// channel authenticated by the Data Plane CA (unless Insecure is set).
		NoClientAuth:  true,
		ServerVersion: "SSH-2.0-rc-datad",
	}
	sshCfg.AddHostKey(hostSigner)

	maxConns := cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = defaultMaxConnections
	}
	maxChans := cfg.MaxChannelsPerConn
	if maxChans <= 0 {
		maxChans = defaultMaxChannelsPerConn
	}
	handshakeTimeout := cfg.HandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	sem := make(chan struct{}, maxConns)

	serveCtx, serveCancel := context.WithCancel(ctx)
	defer serveCancel()
	go func() {
		<-serveCtx.Done()
		_ = listener.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		rawConn, err := listener.Accept()
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
			s.serveConn(ctx, c, sshCfg, requireMTLS, handshakeTimeout, maxChans)
		}(rawConn)
	}
}

func (s *Server) serveConn(
	ctx context.Context,
	rawConn net.Conn,
	sshCfg *ssh.ServerConfig,
	requireMTLS bool,
	handshakeTimeout time.Duration,
	maxChannels int,
) {
	defer func() { _ = rawConn.Close() }()

	if err := rawConn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}

	if requireMTLS {
		tlsConn, ok := rawConn.(*tls.Conn)
		if !ok {
			return
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return
		}
		state := tlsConn.ConnectionState()
		if state.Version < tls.VersionTLS13 ||
			len(state.VerifiedChains) == 0 ||
			len(state.VerifiedChains[0]) == 0 ||
			state.VerifiedChains[0][0].Subject.CommonName != pki.GatewayClientCommonName ||
			state.NegotiatedProtocol != pki.SSHProtocolALPN {
			return
		}
	}

	sshConn, chans, reqs, err := ssh.NewServerConn(rawConn, sshCfg)
	if err != nil {
		return
	}
	defer func() { _ = sshConn.Close() }()

	// Handshake succeeded; clear the handshake deadline for long-lived sessions.
	_ = rawConn.SetDeadline(time.Time{})

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	go func() {
		<-connCtx.Done()
		_ = sshConn.Close()
	}()

	go handleGlobalRequests(reqs)

	chanSem := make(chan struct{}, maxChannels)
	var chanWG sync.WaitGroup

	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			select {
			case chanSem <- struct{}{}:
			default:
				_ = newChan.Reject(ssh.ResourceShortage, "too many concurrent channels")
				continue
			}
			channel, requests, acceptErr := newChan.Accept()
			if acceptErr != nil {
				<-chanSem
				continue
			}
			chanWG.Go(func() {
				defer func() { <-chanSem }()
				s.handleSession(connCtx, channel, requests)
			})
		case "direct-tcpip":
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
				s.handleDirectTCPIP(connCtx, nc)
			}(newChan)
		default:
			_ = newChan.Reject(ssh.UnknownChannelType, fmt.Sprintf("unsupported channel type %q", newChan.ChannelType()))
		}
	}

	// Cancel connCtx BEFORE waiting for channel goroutines so active PTY shells,
	// SFTP servers, and direct-tcpip relays tear down immediately on disconnect.
	connCancel()
	chanWG.Wait()
}

func handleGlobalRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "keepalive@openssh.com":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			// Reject tcpip-forward and any other global requests to prevent
			// unauthorized listener binding.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

type ptyRequestPayload struct {
	Term     string
	Columns  uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
	Modes    string
}

type windowChangePayload struct {
	Columns  uint32
	Rows     uint32
	WidthPx  uint32
	HeightPx uint32
}

type execRequestPayload struct {
	Command string
}

type subsystemRequestPayload struct {
	Name string
}

type envRequestPayload struct {
	Name  string
	Value string
}

type signalRequestPayload struct {
	Signal string
}

type directTCPIPPayload struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

func (s *Server) handleSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()

	var (
		hasPTY  bool
		ptyTerm string
		ptyCols uint16 = 80
		ptyRows uint16 = 24
		envs           = make(map[string]string)
	)

	for req := range requests {
		switch req.Type {
		case "pty-req":
			var p ptyRequestPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				replyIfWanted(req, false)
				continue
			}
			hasPTY = true
			ptyTerm = sanitizeTerm(p.Term)
			ptyCols = clampDimension(p.Columns, 80)
			ptyRows = clampDimension(p.Rows, 24)
			replyIfWanted(req, true)

		case "env":
			var e envRequestPayload
			if err := ssh.Unmarshal(req.Payload, &e); err != nil {
				replyIfWanted(req, false)
				continue
			}
			if isAllowedEnvVar(e.Name, e.Value) && len(envs) < maxEnvEntries {
				val := e.Value
				if e.Name == envTerm {
					val = sanitizeTerm(val)
				}
				envs[e.Name] = val
				replyIfWanted(req, true)
			} else {
				replyIfWanted(req, false)
			}

		case "window-change":
			var w windowChangePayload
			if err := ssh.Unmarshal(req.Payload, &w); err != nil {
				replyIfWanted(req, false)
				continue
			}
			ptyCols = clampDimension(w.Columns, ptyCols)
			ptyRows = clampDimension(w.Rows, ptyRows)
			replyIfWanted(req, true)

		case "shell":
			cmd, args := defaultShellCommand()
			replyIfWanted(req, true)
			s.runSupervisedSession(ctx, channel, requests, cmd, args, hasPTY, ptyTerm, ptyCols, ptyRows, envs)
			return

		case "exec":
			var execReq execRequestPayload
			if err := ssh.Unmarshal(req.Payload, &execReq); err != nil || strings.TrimSpace(execReq.Command) == "" {
				replyIfWanted(req, false)
				continue
			}
			cmd, args := execShellCommand(execReq.Command)
			replyIfWanted(req, true)
			s.runSupervisedSession(ctx, channel, requests, cmd, args, hasPTY, ptyTerm, ptyCols, ptyRows, envs)
			return

		case "subsystem":
			var subReq subsystemRequestPayload
			if err := ssh.Unmarshal(req.Payload, &subReq); err != nil || subReq.Name != "sftp" {
				replyIfWanted(req, false)
				continue
			}
			replyIfWanted(req, true)
			s.runSFTPSubsystem(ctx, channel, requests)
			return

		default:
			replyIfWanted(req, false)
		}
	}
}

func (s *Server) runSupervisedSession(
	ctx context.Context,
	channel ssh.Channel,
	requests <-chan *ssh.Request,
	cmd string,
	args []string,
	hasPTY bool,
	ptyTerm string,
	ptyCols uint16,
	ptyRows uint16,
	envs map[string]string,
) {
	if s.supervisor == nil {
		_ = channel.CloseWrite()
		sendExitStatus(channel, 1)
		return
	}

	s.supervisor.Touch(time.Now())

	mergedEnvs := make(map[string]string, len(envs)+1)
	maps.Copy(mergedEnvs, envs)
	if hasPTY && ptyTerm != "" {
		mergedEnvs[envTerm] = ptyTerm
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	info, sub, err := s.supervisor.StartDataPlaneProcess(sessCtx, rckube.DataPlaneProcessConfig{
		Cmd:   cmd,
		Args:  args,
		Envs:  mergedEnvs,
		Cwd:   s.workspaceDir,
		PTY:   hasPTY,
		Cols:  ptyCols,
		Rows:  ptyRows,
		Stdin: true,
	})
	if err != nil {
		_, _ = fmt.Fprintf(channel.Stderr(), "start process: %v\n", err)
		_ = channel.CloseWrite()
		sendExitStatus(channel, 127)
		return
	}
	defer sub.Close()

	go s.handleSessionControlRequests(requests, info.PID, ptyCols, ptyRows, sessCancel)
	go s.copySessionInput(channel, info.PID, hasPTY)

	for _, chunk := range sub.Replay() {
		if err := writeSessionChunk(channel, chunk, hasPTY); err != nil {
			_, _ = s.supervisor.Stop(info.ID)
			return
		}
	}

	for {
		select {
		case <-sessCtx.Done():
			_, _ = s.supervisor.Stop(info.ID)
			return
		case chunk, ok := <-sub.Chunks():
			if !ok {
				_ = channel.CloseWrite()
				sendExitStatus(channel, normalizeExitCode(sub.Outcome().ExitCode))
				return
			}
			if err := writeSessionChunk(channel, chunk, hasPTY); err != nil {
				_, _ = s.supervisor.Stop(info.ID)
				return
			}
		case <-sub.Exit():
			drainSessionChunks(channel, sub.Chunks(), hasPTY)
			s.supervisor.Touch(time.Now())
			_ = channel.CloseWrite()
			sendExitStatus(channel, normalizeExitCode(sub.Outcome().ExitCode))
			return
		}
	}
}

func (s *Server) handleSessionControlRequests(
	requests <-chan *ssh.Request,
	pid uint32,
	ptyCols uint16,
	ptyRows uint16,
	cancel context.CancelFunc,
) {
	curCols, curRows := ptyCols, ptyRows
	for req := range requests {
		switch req.Type {
		case "window-change":
			var w windowChangePayload
			if ssh.Unmarshal(req.Payload, &w) == nil {
				curCols = clampDimension(w.Columns, curCols)
				curRows = clampDimension(w.Rows, curRows)
				_ = s.supervisor.ResizeDataPlanePTY(pid, "", curCols, curRows)
				replyIfWanted(req, true)
			} else {
				replyIfWanted(req, false)
			}
		case "signal":
			var sig signalRequestPayload
			if ssh.Unmarshal(req.Payload, &sig) == nil {
				if force, ok := mapSSHSignal(sig.Signal); ok {
					_ = s.supervisor.SendDataPlaneSignal(pid, "", force)
					replyIfWanted(req, true)
				} else {
					replyIfWanted(req, false)
				}
			} else {
				replyIfWanted(req, false)
			}
		default:
			replyIfWanted(req, false)
		}
	}
	cancel()
}

func (s *Server) copySessionInput(channel ssh.Channel, pid uint32, hasPTY bool) {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := channel.Read(buf)
		if n > 0 {
			s.supervisor.Touch(time.Now())
			if writeErr := s.supervisor.SendDataPlaneInput(pid, "", buf[:n]); writeErr != nil {
				return
			}
		}
		if readErr != nil {
			if !hasPTY {
				_ = s.supervisor.CloseDataPlaneStdin(pid, "")
			}
			return
		}
	}
}

func writeSessionChunk(channel ssh.Channel, chunk rckube.DataPlaneChunk, hasPTY bool) error {
	if len(chunk.Data) == 0 {
		return nil
	}
	if chunk.Kind == rckube.DataPlaneStreamStderr && !hasPTY {
		_, err := channel.Stderr().Write(chunk.Data)
		return err
	}
	_, err := channel.Write(chunk.Data)
	return err
}

func drainSessionChunks(channel ssh.Channel, chunks <-chan rckube.DataPlaneChunk, hasPTY bool) {
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if err := writeSessionChunk(channel, chunk, hasPTY); err != nil {
				return
			}
		default:
			return
		}
	}
}

func (s *Server) runSFTPSubsystem(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	if s.supervisor != nil {
		s.supervisor.Touch(time.Now())
		defer s.supervisor.Touch(time.Now())
	}

	opts := []sftp.ServerOption{}
	if s.workspaceDir != "" {
		opts = append(opts, sftp.WithServerWorkingDirectory(s.workspaceDir))
	}
	server, err := sftp.NewServer(channel, opts...)
	if err != nil {
		_ = channel.CloseWrite()
		sendExitStatus(channel, 1)
		return
	}

	subCtx, subCancel := context.WithCancel(ctx)
	defer subCancel()

	go func() {
		ssh.DiscardRequests(requests)
		subCancel()
	}()
	go func() {
		<-subCtx.Done()
		_ = server.Close()
	}()

	serveErr := server.Serve()
	_ = server.Close()
	_ = channel.CloseWrite()
	if serveErr != nil && !errors.Is(serveErr, io.EOF) {
		sendExitStatus(channel, 1)
		return
	}
	sendExitStatus(channel, 0)
}

func (s *Server) handleDirectTCPIP(ctx context.Context, newChan ssh.NewChannel) {
	var payload directTCPIPPayload
	if err := ssh.Unmarshal(newChan.ExtraData(), &payload); err != nil {
		_ = newChan.Reject(ssh.Prohibited, "invalid direct-tcpip payload")
		return
	}

	// Security: only allow loopback forwarding inside the Pod so an SSH client
	// cannot pivot through the Pod as an SSRF proxy to cluster or metadata IPs.
	dialHost, ok := normalizeLoopbackHost(payload.DestAddr)
	if !ok || payload.DestPort == 0 || payload.DestPort > 65535 {
		_ = newChan.Reject(ssh.Prohibited, "direct-tcpip is restricted to loopback destinations")
		return
	}

	if s.supervisor != nil {
		s.supervisor.Touch(time.Now())
	}

	targetAddr := net.JoinHostPort(dialHost, fmt.Sprintf("%d", payload.DestPort))
	dialer := &net.Dialer{Timeout: defaultDialTimeout}
	upstream, err := dialer.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, fmt.Sprintf("dial %s: %v", targetAddr, err))
		return
	}

	channel, requests, err := newChan.Accept()
	if err != nil {
		_ = upstream.Close()
		return
	}

	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			_ = upstream.Close()
			_ = channel.Close()
		})
	}
	defer closeAll()

	done := make(chan struct{})
	defer close(done)

	go func() {
		ssh.DiscardRequests(requests)
		closeAll()
	}()
	go func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, channel)
		if tcpConn, ok := upstream.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(channel, upstream)
		_ = channel.CloseWrite()
	}()
	wg.Wait()
}

// normalizeLoopbackHost strictly accepts loopback destinations ("localhost",
// "127.0.0.1", "::1", or any 127.0.0.0/8 IP) and returns a literal IP address
// so DNS rebinding cannot redirect the dial to a non-loopback address.
func normalizeLoopbackHost(host string) (string, bool) {
	trimmed := strings.TrimSpace(host)
	if strings.EqualFold(trimmed, "localhost") {
		return loopbackIPv4, true
	}
	ip := net.ParseIP(trimmed)
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return ip.String(), true
}

func defaultShellCommand() (string, []string) {
	if runtime.GOOS == osWindows {
		return "cmd.exe", nil
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash", []string{"-l"}
	}
	return "/bin/sh", nil
}

func execShellCommand(command string) (string, []string) {
	if runtime.GOOS == osWindows {
		return "cmd.exe", []string{"/C", command}
	}
	return "/bin/sh", []string{"-c", command}
}

func isAllowedEnvVar(name, value string) bool {
	if name == "" || len(value) > maxEnvValueLen {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	switch name {
	case envTerm, "LANG", "COLORTERM":
		return true
	}
	if strings.HasPrefix(name, "LC_") && len(name) <= 32 {
		for _, r := range name[3:] {
			if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
				return false
			}
		}
		return true
	}
	return false
}

func mapSSHSignal(name string) (force bool, ok bool) {
	switch name {
	case "KILL":
		return true, true
	case "TERM", "INT", "HUP", "QUIT":
		return false, true
	default:
		return false, false
	}
}

func normalizeExitCode(code int32) uint32 {
	if code < 0 {
		return 255
	}
	return uint32(code)
}

func sanitizeTerm(term string) string {
	trimmed := strings.TrimSpace(term)
	if trimmed == "" || len(trimmed) > 64 {
		return "xterm-256color"
	}
	for _, r := range trimmed {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' && r != '+' {
			return "xterm-256color"
		}
	}
	return trimmed
}

func clampDimension(val uint32, fallback uint16) uint16 {
	if val == 0 {
		return fallback
	}
	if val > maxPTYDimension {
		return maxPTYDimension
	}
	return uint16(val)
}

func replyIfWanted(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

func sendExitStatus(channel ssh.Channel, code uint32) {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], code)
	_, _ = channel.SendRequest("exit-status", false, payload[:])
}
