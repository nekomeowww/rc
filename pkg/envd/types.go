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

// Package envd provides reusable E2B envd 0.11.0 wire-contract constants,
// REST payload types, signed file URL helpers, and header parsers.
package envd

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	// Version is the pinned E2B envd wire contract version advertised in X-Envd-Version.
	Version = "0.11.0"
	// DefaultPort is the standard E2B envd TCP port.
	DefaultPort = 49983
	// DefaultUser is the default username used by E2B SDKs.
	DefaultUser = "user"
	// AccessTokenHeader is the standard E2B envd authentication header.
	AccessTokenHeader = "X-Access-Token"
	// VersionHeader advertises the envd protocol version.
	VersionHeader = "X-Envd-Version"

	metadataHeaderPrefix = "X-Metadata-"
	metadataXattrPrefix  = "user.e2b."
	maxMetadataKeyLen    = 246
	maxMetadataTotalLen  = 4096
)

// RESTEntryInfo is returned by POST /files for each uploaded file.
type RESTEntryInfo struct {
	Path     string            `json:"path"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// RESTError is the standard JSON error payload for envd REST endpoints.
type RESTError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// WriteJSONError writes a JSON RESTError with status code.
func WriteJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(RESTError{
		Message: message,
		Code:    status,
	})
}

// SignFileURL computes an E2B-compatible v1 file URL signature.
// Format matches upstream e2b-dev/infra: "v1_" + base64url(sha256("path:op:user:token[:exp]")).
func SignFileURL(path, operation, username, token string, expiration int64) string {
	if username == "" {
		username = DefaultUser
	}
	raw := fmt.Sprintf("%s:%s:%s:%s", path, operation, username, token)
	if expiration > 0 {
		raw = fmt.Sprintf("%s:%d", raw, expiration)
	}
	sum := sha256.Sum256([]byte(raw))
	return "v1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// NegotiateEncoding parses Accept-Encoding for GET /files, supporting identity and gzip.
func NegotiateEncoding(accept string) (useGzip bool, err error) {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return false, nil
	}
	wantsGzip := false
	acceptsIdentity := false
	for part := range strings.SplitSeq(accept, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		encoding, params, _ := strings.Cut(part, ";")
		encoding = strings.ToLower(strings.TrimSpace(encoding))
		rejected := strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") &&
			!strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0.")
		switch encoding {
		case "gzip":
			if !rejected {
				wantsGzip = true
			}
		case "identity", "*":
			if !rejected {
				acceptsIdentity = true
			}
		}
	}
	if wantsGzip {
		return true, nil
	}
	if acceptsIdentity {
		return false, nil
	}
	return false, errors.New("no acceptable encoding found, supported: [identity, gzip]")
}

// ParseMetadataHeaders extracts and validates X-Metadata-* headers per envd.yaml.
func ParseMetadataHeaders(header http.Header) (map[string]string, error) {
	var result map[string]string
	total := 0
	for name, values := range header {
		prefixLen := len(metadataHeaderPrefix)
		if len(name) <= prefixLen || !strings.EqualFold(name[:prefixLen], metadataHeaderPrefix) {
			continue
		}
		key := strings.ToLower(name[len(metadataHeaderPrefix):])
		if len(key) == 0 || len(key) > maxMetadataKeyLen {
			return nil, fmt.Errorf("invalid metadata key %q", key)
		}
		val := ""
		if len(values) > 0 {
			val = values[0]
		}
		if !isPrintableASCII(key) || !isPrintableASCII(val) {
			return nil, fmt.Errorf("metadata key %q and value must be printable US-ASCII", key)
		}
		total += len(metadataXattrPrefix) + len(key) + len(val)
		if total > maxMetadataTotalLen {
			return nil, fmt.Errorf("total metadata exceeds %d bytes", maxMetadataTotalLen)
		}
		if result == nil {
			result = make(map[string]string)
		}
		result[key] = val
	}
	return result, nil
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}
