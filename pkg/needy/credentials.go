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

package needy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/nekomeowww/xo"
	"sigs.k8s.io/yaml"
)

const (
	// mintedCredentialPrefix starts a runner credential that the daemon
	// mints: needy-runner.<name>.<64 hex>.
	mintedCredentialPrefix = "needy-runner."
	// secretBytes is the randomness of a static secret: needy refuses fewer
	// than 32 bytes.
	secretBytes = 32
)

// Access is a static client's grant.
type Access string

// needy's client grants.
const (
	AccessRuns  Access = "runs"
	AccessAdmin Access = "admin"
)

// NewSecret returns a static runner credential or client token: 32 random
// bytes in unpadded base64url. needy matches it by its SHA-256 in the
// runners or clients file, so it carries no name. Use each secret for one
// entry only: a secret in both files is refused.
func NewSecret() (string, error) {
	secret, err := xo.RandBytes(secretBytes)
	if err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(secret), nil
}

// IsSecret reports whether value is a static secret that needy accepts: at
// least 32 bytes, in base64url as NewSecret writes it.
func IsSecret(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)

	return err == nil && len(decoded) >= secretBytes
}

// IsMintedCredential reports whether credential is one that the daemon
// minted for runner.
func IsMintedCredential(credential, runner string) bool {
	return strings.HasPrefix(credential, mintedCredentialPrefix+runner+".")
}

// Hash is how the runners and clients files name a credential or token:
// sha256: and the SHA-256 of the whole string.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return "sha256:" + hex.EncodeToString(sum[:])
}

// StaticRunner is one entry of the daemon's runners file.
type StaticRunner struct {
	Name           string            `json:"name"`
	CredentialHash string            `json:"credential_hash"`
	Roles          []Role            `json:"roles"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// StaticClient is one entry of the daemon's clients file.
type StaticClient struct {
	Name      string `json:"name"`
	Access    Access `json:"access"`
	TokenHash string `json:"token_hash"`
}

// RunnersFile renders the daemon's runners file. Entries are sorted by name,
// so an unchanged set renders the same bytes.
func RunnersFile(runners []StaticRunner) ([]byte, error) {
	return renderSorted("runners", runners, func(runner StaticRunner) string { return runner.Name })
}

// ClientsFile renders the daemon's clients file, sorted like RunnersFile.
func ClientsFile(clients []StaticClient) ([]byte, error) {
	return renderSorted("clients", clients, func(client StaticClient) string { return client.Name })
}

func renderSorted[T any](key string, entries []T, name func(T) string) ([]byte, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b T) int {
		return strings.Compare(name(a), name(b))
	})
	if sorted == nil {
		sorted = []T{}
	}
	data, err := yaml.Marshal(map[string][]T{key: sorted})
	if err != nil {
		return nil, fmt.Errorf("render needy %s file: %w", key, err)
	}

	return data, nil
}
