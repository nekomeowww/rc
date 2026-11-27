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

// Package envdconnectrpc implements the E2B envd 0.11.0 ConnectRPC and REST
// wire contract inside rc-kube.
package envdconnectrpc

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/labstack/echo/v4"

	"github.com/nekomeowww/rc/apis/sdk/go/envd/filesystem/filesystemconnect"
	"github.com/nekomeowww/rc/apis/sdk/go/envd/process/processconnect"
	"github.com/nekomeowww/rc/internal/datad/pki"
	"github.com/nekomeowww/rc/internal/rckube"
	"github.com/nekomeowww/rc/pkg/envd"
)

const (
	// KeepAliveInterval is the interval for keepalive frames on streaming RPCs.
	KeepAliveInterval = 15 * time.Second
)

// Config configures the in-Pod envd Data Plane server.
type Config struct {
	ListenAddr string
	CAFile     string
	CertFile   string
	KeyFile    string
	// Insecure disables mTLS and serves HTTP/1.1 + h2c. Intended only for
	// local testing or an already-authenticated transport such as a Darwin exec bridge.
	Insecure    bool
	HomeDir     string
	RuntimeUser string
}

// Server serves the E2B envd REST and ConnectRPC endpoints backed by a Supervisor.
type Server struct {
	supervisor  *rckube.Supervisor
	homeDir     string
	runtimeUser string
	handler     http.Handler

	watcherMu sync.Mutex
	watchers  map[string]*managedWatcher
	nextWatch uint64

	metaMu   sync.RWMutex
	metadata map[string]map[string]string
}

// NewServer constructs an envd server bound to supervisor.
func NewServer(supervisor *rckube.Supervisor, homeDir, runtimeUser string) *Server {
	if homeDir == "" && supervisor != nil {
		homeDir = supervisor.HomeDir()
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	if runtimeUser == "" {
		if current, err := user.Current(); err == nil {
			runtimeUser = current.Username
		}
	}

	s := &Server{
		supervisor:  supervisor,
		homeDir:     homeDir,
		runtimeUser: runtimeUser,
		watchers:    make(map[string]*managedWatcher),
		metadata:    make(map[string]map[string]string),
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(s.activityMiddleware)

	e.GET("/health", func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	})
	e.POST("/init", func(c echo.Context) error {
		envd.WriteJSONError(c.Response().Writer, http.StatusNotFound, "endpoint not implemented")
		return nil
	})
	e.GET("/files", echo.WrapHandler(http.HandlerFunc(s.handleDownloadFile)))
	e.POST("/files", echo.WrapHandler(http.HandlerFunc(s.handleUploadFiles)))

	fsPath, fsHandler := filesystemconnect.NewFilesystemHandler(&filesystemHandler{server: s})
	e.Any(fsPath+"*", echo.WrapHandler(fsHandler))

	procPath, procHandler := processconnect.NewProcessHandler(&processHandler{server: s})
	e.Any(procPath+"*", echo.WrapHandler(procHandler))

	s.handler = e
	return s
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Serve runs the Data Plane listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, cfg Config) error {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", envd.DefaultPort)
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on data plane address %s: %w", cfg.ListenAddr, err)
	}
	return s.ServeListener(ctx, listener, cfg)
}

// ServeListener serves on an already-bound listener (useful for tests binding :0).
func (s *Server) ServeListener(ctx context.Context, listener net.Listener, cfg Config) error {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if !cfg.Insecure {
		if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
			return errors.New("data plane mTLS requires ca-file, cert-file, and key-file")
		}
		tlsConfig, err := pki.ReloadingServerTLSConfig(ctx, cfg.CAFile, cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return err
		}
		listener = tls.NewListener(listener, tlsConfig)
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}

	httpServer := &http.Server{
		Handler:           s.handler,
		Protocols:         protocols,
		ReadHeaderTimeout: 30 * time.Second,
	}

	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		s.closeAllWatchers()
	})
	defer stop()

	err := httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Server) activityMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(envd.VersionHeader, envd.Version)
		isHealth := c.Request().Method == http.MethodGet && c.Request().URL.Path == "/health"
		if !isHealth && s.supervisor != nil {
			s.supervisor.Touch(time.Now())
			defer func() {
				s.supervisor.Touch(time.Now())
			}()
		}
		return next(c)
	}
}

// validateUser enforces the spec's user rule: empty, "user" (E2B default), or
// the runtime user are allowed; "root" and any other user are rejected.
func (s *Server) validateUser(requested string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == envd.DefaultUser {
		return nil
	}
	if requested == "root" {
		return fmt.Errorf("user %q is not allowed", requested)
	}
	if s.runtimeUser != "" && requested == s.runtimeUser {
		return nil
	}
	return fmt.Errorf("user %q is not allowed", requested)
}

// validateRPCUser extracts and validates the Basic auth user from ConnectRPC headers.
func (s *Server) validateRPCUser(header http.Header) error {
	auth := header.Get("Authorization")
	if auth == "" {
		return nil
	}
	if !strings.HasPrefix(auth, "Basic ") {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied, errors.New("invalid Authorization header"))
	}
	username, _, _ := strings.Cut(string(decoded), ":")
	if err := s.validateUser(username); err != nil {
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	return nil
}

// resolvePath implements upstream envd path resolution:
// - "" -> homeDir
// - "~" or "~/..." -> homeDir
// - relative -> joined to homeDir
// - absolute -> cleaned as-is
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
	return filepath.Clean(filepath.Join(s.homeDir, raw))
}

func (s *Server) setFileMetadata(path string, meta map[string]string) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if len(meta) == 0 {
		delete(s.metadata, path)
		return
	}
	copied := make(map[string]string, len(meta))
	maps.Copy(copied, meta)
	s.metadata[path] = copied
}

func (s *Server) getFileMetadata(path string) map[string]string {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	meta := s.metadata[path]
	if len(meta) == 0 {
		return nil
	}
	copied := make(map[string]string, len(meta))
	maps.Copy(copied, meta)
	return copied
}

func (s *Server) deleteFileMetadata(path string) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	delete(s.metadata, path)
}

func (s *Server) moveFileMetadata(src, dst string) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if meta, ok := s.metadata[src]; ok {
		s.metadata[dst] = meta
		delete(s.metadata, src)
	}
}
