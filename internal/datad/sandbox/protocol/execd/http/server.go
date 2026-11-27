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

// Package execdhttp implements the OpenSandbox execd v1.0.0 HTTP/SSE data-plane
// protocol inside rc-kube, backed by rckube.Supervisor.
package execdhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/nekomeowww/rc/internal/datad/pki"
	execdws "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/execd/ws"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/pkg/execd"
)

// Config configures the in-Pod execd Data Plane server.
type Config struct {
	ListenAddr string
	CAFile     string
	CertFile   string
	KeyFile    string
	// Insecure disables mTLS and serves HTTP/1.1 + h2c. Intended only for
	// local testing or an already-authenticated transport such as an exec bridge.
	Insecure     bool
	HomeDir      string
	WorkspaceDir string
	RuntimeUser  string
}

type commandRecord struct {
	id         string
	content    string
	startedAt  time.Time
	finishedAt *time.Time
	running    bool
	exitCode   *int32
	errMessage string
}

// Server serves the OpenSandbox execd REST + SSE endpoints backed by a Supervisor.
type Server struct {
	supervisor   *rckube.Supervisor
	homeDir      string
	workspaceDir string
	runtimeUser  string
	handler      http.Handler

	cmdMu    sync.RWMutex
	commands map[string]*commandRecord
}

// NewServer constructs an execd server bound to supervisor.
func NewServer(supervisor *rckube.Supervisor, homeDir, workspaceDir, runtimeUser string) *Server {
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
	if runtimeUser == "" {
		if current, err := user.Current(); err == nil {
			runtimeUser = current.Username
		}
	}

	s := &Server{
		supervisor:   supervisor,
		homeDir:      homeDir,
		workspaceDir: workspaceDir,
		runtimeUser:  runtimeUser,
		commands:     make(map[string]*commandRecord),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("POST /command", s.handleRunCommand)
	mux.HandleFunc("DELETE /command", s.handleInterruptCommand)
	mux.HandleFunc("GET /command/", s.handleCommandSubroute)

	mux.HandleFunc("GET /files/info", s.handleFilesInfo)
	mux.HandleFunc("DELETE /files", s.handleRemoveFiles)
	mux.HandleFunc("POST /files/permissions", s.handleChmodFiles)
	mux.HandleFunc("POST /files/mv", s.handleRenameFiles)
	mux.HandleFunc("GET /files/search", s.handleSearchFiles)
	mux.HandleFunc("POST /files/replace", s.handleReplaceContent)
	mux.HandleFunc("POST /files/upload", s.handleUploadFiles)
	mux.HandleFunc("GET /files/download", s.handleDownloadFile)
	mux.HandleFunc("GET /directories/list", s.handleListDirectory)
	mux.HandleFunc("POST /directories", s.handleMakeDirs)
	mux.HandleFunc("DELETE /directories", s.handleRemoveDirs)
	mux.Handle("GET /pty", execdws.NewHandler(supervisor, s.workspaceDir))

	s.handler = s.wrapHandler(mux)
	return s
}

// Handler returns the root http.Handler (supporting HTTP/1.1 and h2c).
func (s *Server) Handler() http.Handler {
	return h2c.NewHandler(s.handler, &http2.Server{})
}

// Serve runs the execd listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", execd.DefaultPort)
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on execd address %s: %w", cfg.ListenAddr, err)
	}
	return s.ServeListener(ctx, listener, cfg)
}

// ServeListener serves on an already-bound listener (useful for tests binding :0).
func (s *Server) ServeListener(ctx context.Context, listener net.Listener, cfg Config) error {
	var tlsConfig *tls.Config
	if !cfg.Insecure {
		if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
			return errors.New("execd mTLS requires ca-file, cert-file, and key-file")
		}
		var err error
		tlsConfig, err = pki.ReloadingServerTLSConfig(cfg.CAFile, cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return err
		}
		listener = tls.NewListener(listener, tlsConfig)
	}

	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	if tlsConfig != nil {
		httpServer.Handler = s.handler
		if err := http2.ConfigureServer(httpServer, &http2.Server{}); err != nil {
			return fmt.Errorf("configure HTTP/2 on execd server: %w", err)
		}
	}

	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	})
	defer stop()

	err := httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Server) wrapHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(execd.VersionHeader, execd.Version)
		isPing := r.Method == http.MethodGet && r.URL.Path == "/ping"
		if !isPing && s.supervisor != nil {
			s.supervisor.Touch(time.Now())
			defer func() {
				s.supervisor.Touch(time.Now())
			}()
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) resolvePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "~" {
		return s.homeDir
	}
	if after, ok := strings.CutPrefix(raw, "~/"); ok {
		return filepath.Clean(filepath.Join(s.homeDir, after))
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(s.workspaceDir, raw))
}
