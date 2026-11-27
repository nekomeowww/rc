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
// It authenticates external E2B envd requests by Workspace access token (or
// signed /files URL) and forwards them over mTLS to the Workspace runtime Pod.
package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

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
	Namespace    string
	Name         string
	Ready        bool
	State        string
	ExecdAddress string // host:port of the primary OpenSandbox execd listener (44772)
	Address      string // host:port of the E2B envd-compatible listener (49983)
	ServerName   string // expected mTLS ServerName (<name>.<namespace>.workspace.rc.ayaka.io)
	AccessToken  string
	Insecure     bool
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
}

// NewServer creates a Data Plane Gateway server.
func NewServer(resolver Resolver, certProvider ClientCertProvider) *Server {
	return &Server{
		resolver:     resolver,
		certProvider: certProvider,
		now:          time.Now,
	}
}

// Handler returns the HTTP/1.1 + h2c handler for the gateway.
func (s *Server) Handler() http.Handler {
	return h2c.NewHandler(http.HandlerFunc(s.serveHTTP), &http2.Server{})
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/healthz" || r.URL.Path == "/readyz") {
		w.WriteHeader(http.StatusOK)
		return
	}

	namespace, name, remainder, ok := parseWorkspaceRoute(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "route must match /namespaces/<namespace>/workspaces/<name>/...")
		return
	}

	target, err := s.resolver.Resolve(r.Context(), namespace, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("workspace %s/%s not found or data plane is disabled", namespace, name))
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Sprintf("resolve workspace %s/%s: %v", namespace, name, err))
		return
	}

	protocol, upstreamRemainder := splitProtocolPrefix(remainder)
	isHealth := (protocol == protocolEnvd && r.Method == http.MethodGet && upstreamRemainder == "/health") ||
		(protocol == protocolExecd && r.Method == http.MethodGet && upstreamRemainder == "/ping")
	if !isHealth {
		if !s.authenticate(r, protocol, upstreamRemainder, target.AccessToken) {
			writeError(w, http.StatusUnauthorized, "invalid or missing access token")
			return
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
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("workspace %s/%s is not ready (state: %s)", namespace, name, state))
		return
	}

	transport, scheme, err := s.transportFor(r.Context(), target)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("configure upstream transport: %v", err))
		return
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
			pr.Out.Header.Del("X-Access-Token")
			pr.Out.Header.Del("X-EXECD-ACCESS-TOKEN")
			if strings.HasPrefix(pr.Out.Header.Get("Authorization"), "Bearer ") {
				pr.Out.Header.Del("Authorization")
			}
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
			writeError(rw, http.StatusBadGateway, fmt.Sprintf("upstream data plane error: %v", proxyErr))
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) transportFor(ctx context.Context, target Target) (http.RoundTripper, string, error) {
	if target.Insecure {
		return &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(dialCtx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(dialCtx, network, addr)
			},
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

// splitProtocolPrefix routes `/execd/...` to the primary OpenSandbox execd
// listener (44772) and `/envd/...` (or unprefixed paths for E2B compatibility)
// to the E2B envd listener (49983).
func splitProtocolPrefix(remainder string) (protocol, path string) {
	if remainder == "/"+protocolExecd {
		return protocolExecd, "/"
	}
	if rest, ok := strings.CutPrefix(remainder, "/"+protocolExecd+"/"); ok {
		return protocolExecd, "/" + rest
	}
	if remainder == "/"+protocolEnvd {
		return protocolEnvd, "/"
	}
	if rest, ok := strings.CutPrefix(remainder, "/"+protocolEnvd+"/"); ok {
		return protocolEnvd, "/" + rest
	}
	return protocolEnvd, remainder
}

// authenticate checks caller tokens in constant time, or verifies an E2B
// signed /files URL when signature query parameters are provided.
func (s *Server) authenticate(r *http.Request, protocol, remainder, expectedToken string) bool {
	if expectedToken == "" {
		return false
	}
	if protocol == protocolExecd {
		if provided := r.Header.Get("X-EXECD-ACCESS-TOKEN"); provided != "" {
			return subtle.ConstantTimeCompare([]byte(provided), []byte(expectedToken)) == 1
		}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token := strings.TrimPrefix(auth, "Bearer ")
			return subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) == 1
		}
	}
	if provided := r.Header.Get("X-Access-Token"); provided != "" {
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

// Keep pkg/execd header constant referenced cleanly.
var _ = execd.AccessTokenHeader

// parseWorkspaceRoute parses `/namespaces/<ns>/workspaces/<name>[/<rest>]`.
func parseWorkspaceRoute(path string) (namespace, name, remainder string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 5)
	if len(parts) < 4 || parts[0] != "namespaces" || parts[2] != "workspaces" {
		return "", "", "", false
	}
	namespace, name = parts[1], parts[3]
	if namespace == "" || name == "" {
		return "", "", "", false
	}
	if len(parts) == 4 || parts[4] == "" {
		remainder = "/"
	} else {
		remainder = "/" + parts[4]
	}
	return namespace, name, remainder, true
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": message,
	})
}
