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

// Package gateway implements the Workspace Data Plane Gateway.
// It authenticates external execd and envd requests by Workspace access token
// (or signed /files URL) and forwards them over mTLS to the Workspace runtime Pod.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/nekomeowww/rc/internal/datad/pki"
	"github.com/nekomeowww/rc/pkg/envd"
	"github.com/nekomeowww/rc/pkg/execd"
)

// ErrNotFound signals that the Workspace does not exist or has no Data Plane enabled.
var ErrNotFound = errors.New("workspace data plane not found")

const (
	protocolExecd = "execd"
	protocolEnvd  = "envd"
)

// Target describes the resolved runtime endpoints and authentication token for a Workspace.
type Target struct {
	Namespace         string
	Name              string
	Ready             bool
	State             string
	ExecdAddress      string   // host:port of the primary OpenSandbox execd listener (44772)
	Address           string   // host:port of the E2B envd-compatible listener (49983)
	SSHAddress        string   // host:port of the in-Pod mTLS SSH listener (2222)
	SSHEnabled        bool     // true when spec.dataPlane.ssh.enabled is true
	SSHAuthorizedKeys []string // OpenSSH authorized_keys entries allowed for this Workspace
	ServerName        string   // expected mTLS ServerName (<name>.<namespace>.workspace.rc.ayaka.io)
	AccessToken       string
	Insecure          bool
}

// Resolver looks up the live routing target and current access token for a Workspace.
type Resolver interface {
	Resolve(ctx context.Context, namespace, name string) (Target, error)
}

// ClientCertProvider returns the current CA and gateway client certificate PEMs
// used to establish mTLS connections to Workspace runtimes.
type ClientCertProvider func(ctx context.Context) (caPEM, certPEM, keyPEM []byte, err error)

// Server routes external Data Plane requests to Workspace runtime Pods.
type Server struct {
	resolver     Resolver
	certProvider ClientCertProvider
	now          func() time.Time
	echo         *echo.Echo
}

// NewServer creates a Data Plane Gateway server using Echo routing.
func NewServer(resolver Resolver, certProvider ClientCertProvider) *Server {
	s := &Server{
		resolver:     resolver,
		certProvider: certProvider,
		now:          time.Now,
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.GET("/healthz", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	e.GET("/readyz", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	ws := e.Group("/namespaces/:namespace/workspaces/:name")
	ws.Any("/execd", s.proxyHandler(protocolExecd))
	ws.Any("/execd/*", s.proxyHandler(protocolExecd))
	ws.Any("/envd", s.proxyHandler(protocolEnvd))
	ws.Any("/envd/*", s.proxyHandler(protocolEnvd))
	ws.Any("/*", s.proxyHandler(protocolEnvd))

	s.echo = e
	return s
}

// Handler returns the root http.Handler for the gateway.
func (s *Server) Handler() http.Handler {
	return s.echo
}

func (s *Server) proxyHandler(protocol string) echo.HandlerFunc {
	return func(c echo.Context) error {
		w, r := c.Response().Writer, c.Request()
		namespace := c.Param("namespace")
		name := c.Param("name")
		upstreamRemainder := "/" + strings.TrimPrefix(c.Param("*"), "/")

		target, err := s.resolver.Resolve(r.Context(), namespace, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeError(w, http.StatusNotFound,
					fmt.Sprintf("workspace %s/%s not found or data plane is disabled", namespace, name))
				return nil
			}
			writeError(w, http.StatusBadGateway, fmt.Sprintf("resolve workspace %s/%s: %v", namespace, name, err))
			return nil
		}

		isHealth := (protocol == protocolEnvd && r.Method == http.MethodGet && upstreamRemainder == "/health") ||
			(protocol == protocolExecd && r.Method == http.MethodGet && upstreamRemainder == "/ping")
		if !isHealth {
			if !s.authenticate(r, protocol, upstreamRemainder, target.AccessToken) {
				writeError(w, http.StatusUnauthorized, "invalid or missing access token")
				return nil
			}
		}

		upstreamAddr := target.Address
		if protocol == protocolExecd {
			upstreamAddr = target.ExecdAddress
		}
		if !target.Ready || upstreamAddr == "" {
			state := target.State
			if state == "" {
				state = "NotReady"
			}
			writeError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("workspace %s/%s is not ready (state: %s)", namespace, name, state))
			return nil
		}

		transport, scheme, err := s.transportFor(r.Context(), target)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("configure upstream transport: %v", err))
			return nil
		}

		upstreamURL := &url.URL{
			Scheme: scheme,
			Host:   upstreamAddr,
		}
		proxy := &httputil.ReverseProxy{
			Transport:     transport,
			FlushInterval: -1,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstreamURL)
				pr.Out.URL.Path = upstreamRemainder
				pr.Out.URL.RawPath = ""
				pr.Out.Host = upstreamURL.Host
				// Never forward caller access tokens into the Workspace runtime Pod.
				pr.Out.Header.Del(envd.AccessTokenHeader)
				pr.Out.Header.Del(execd.AccessTokenHeader)
				if strings.HasPrefix(pr.Out.Header.Get("Authorization"), "Bearer ") {
					pr.Out.Header.Del("Authorization")
				}
			},
			ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
				writeError(rw, http.StatusBadGateway, fmt.Sprintf("upstream data plane error: %v", proxyErr))
			},
		}
		proxy.ServeHTTP(w, r)
		return nil
	}
}

func (s *Server) transportFor(ctx context.Context, target Target) (http.RoundTripper, string, error) {
	if target.Insecure {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		return &http.Transport{
			Protocols: protocols,
		}, "http", nil
	}
	if s.certProvider == nil {
		return nil, "", errors.New("mTLS client certificate provider is not configured")
	}
	caPEM, certPEM, keyPEM, err := s.certProvider(ctx)
	if err != nil {
		return nil, "", err
	}
	serverName := target.ServerName
	if serverName == "" {
		serverName = pki.WorkspaceServerName(target.Namespace, target.Name)
	}
	tlsCfg, err := pki.ClientTLSConfig(caPEM, certPEM, keyPEM, serverName)
	if err != nil {
		return nil, "", err
	}
	return &http.Transport{
		TLSClientConfig:   tlsCfg,
		ForceAttemptHTTP2: true,
	}, "https", nil
}

// authenticate checks caller tokens in constant time, or verifies an E2B
// signed /files URL when signature query parameters are provided.
func (s *Server) authenticate(r *http.Request, protocol, remainder, expectedToken string) bool {
	if expectedToken == "" {
		return false
	}
	if protocol == protocolExecd {
		if provided := r.Header.Get(execd.AccessTokenHeader); provided != "" {
			return subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) == 1
		}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			return subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) == 1
		}
	}
	if provided := r.Header.Get(envd.AccessTokenHeader); provided != "" {
		return subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) == 1
	}
	if protocol == protocolEnvd && remainder == "/files" {
		return s.verifySignedFileURL(r, expectedToken)
	}
	return false
}

// SignFileURL computes an E2B-compatible v1 file URL signature.
func SignFileURL(path, operation, username, token string, expiration int64) string {
	return envd.SignFileURL(path, operation, username, token, expiration)
}

func (s *Server) verifySignedFileURL(r *http.Request, token string) bool {
	q := r.URL.Query()
	sig := q.Get("signature")
	if sig == "" {
		return false
	}
	var exp int64
	if rawExp := q.Get("signature_expiration"); rawExp != "" {
		parsed, err := strconv.ParseInt(rawExp, 10, 64)
		if err != nil || parsed <= 0 {
			return false
		}
		if s.now().Unix() > parsed {
			return false
		}
		exp = parsed
	}
	op := "read"
	if r.Method == http.MethodPost {
		op = "write"
	}
	path := q.Get("path")
	user := q.Get("username")
	expected := envd.SignFileURL(path, op, user, token, exp)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) == 1
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": message,
	})
}
