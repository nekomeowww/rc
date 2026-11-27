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

	"github.com/labstack/echo/v4"

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
	echo         *echo.Echo

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

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(s.activityMiddleware)

	e.GET("/ping", s.handlePing)
	e.POST("/command", s.handleRunCommand)
	e.DELETE("/command", s.handleInterruptCommand)
	e.GET("/command/status/:id", s.handleCommandStatus)
	e.GET("/command/:id/logs", s.handleCommandLogs)

	e.GET("/files/info", s.handleFilesInfo)
	e.DELETE("/files", s.handleRemoveFiles)
	e.POST("/files/permissions", s.handleChmodFiles)
	e.POST("/files/mv", s.handleRenameFiles)
	e.GET("/files/search", s.handleSearchFiles)
	e.POST("/files/replace", s.handleReplaceContent)
	e.POST("/files/upload", s.handleUploadFiles)
	e.GET("/files/download", s.handleDownloadFile)
	e.GET("/directories/list", s.handleListDirectory)
	e.POST("/directories", s.handleMakeDirs)
	e.DELETE("/directories", s.handleRemoveDirs)
	e.GET("/pty", echo.WrapHandler(execdws.NewHandler(supervisor, s.workspaceDir)))

	s.echo = e
	return s
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	return s.echo
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
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	if !cfg.Insecure {
		if cfg.CAFile == "" || cfg.CertFile == "" || cfg.KeyFile == "" {
			return errors.New("execd mTLS requires ca-file, cert-file, and key-file")
		}
		tlsConfig, err := pki.ReloadingServerTLSConfig(cfg.CAFile, cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return err
		}
		listener = tls.NewListener(listener, tlsConfig)
		protocols.SetHTTP2(true)
	} else {
		protocols.SetUnencryptedHTTP2(true)
	}

	httpServer := &http.Server{
		Handler:           s.echo,
		Protocols:         protocols,
		ReadHeaderTimeout: 30 * time.Second,
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

func (s *Server) activityMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(execd.VersionHeader, execd.Version)
		isPing := c.Request().Method == http.MethodGet && c.Request().URL.Path == "/ping"
		if !isPing && s.supervisor != nil {
			s.supervisor.Touch(time.Now())
			defer func() {
				s.supervisor.Touch(time.Now())
			}()
		}
		return next(c)
	}
}

func (s *Server) handlePing(c echo.Context) error {
	return c.NoContent(http.StatusOK)
}

func (s *Server) resolvePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "~" {
		return s.homeDir
	}
	if rest, ok := strings.CutPrefix(raw, "~/"); ok {
		return filepath.Clean(filepath.Join(s.homeDir, rest))
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Clean(filepath.Join(s.workspaceDir, raw))
}
