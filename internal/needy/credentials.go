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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	runnerCredentialPrefix = "needy-runner."
	// clientTokenPrefix starts a static client's token:
	// needy-client.static-<name>.<64 hex>.
	clientTokenPrefix = "needy-client.static-"
	// runnerNameMaxLength is needy's limit: a runner name is a DNS label.
	runnerNameMaxLength = 63
)

// NewRunnerCredential returns a credential in needy's format:
// needy-runner.<name>.<64 hex>. The daemon keeps only its SHA-256.
func NewRunnerCredential(name string) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate runner credential: %w", err)
	}

	return runnerCredentialPrefix + name + "." + hex.EncodeToString(secret), nil
}

// NewClientToken returns a static client token in needy's format. The
// daemon learns only its hash, through the clients file.
func NewClientToken(name string) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate client token: %w", err)
	}

	return clientTokenPrefix + name + "." + hex.EncodeToString(secret), nil
}

// TokenMatchesClient reports whether token is a static token of client.
func TokenMatchesClient(token, client string) bool {
	return strings.HasPrefix(token, clientTokenPrefix+client+".")
}

// CredentialHash is the value of credential_hash in the runners file and of
// token_hash in the clients file.
func CredentialHash(credential string) string {
	sum := sha256.Sum256([]byte(credential))

	return "sha256:" + hex.EncodeToString(sum[:])
}

// CredentialMatchesRunner reports whether credential names runner.
func CredentialMatchesRunner(credential, runner string) bool {
	return strings.HasPrefix(credential, runnerCredentialPrefix+runner+".")
}

// WorkspaceRunnerName is the executor runner name of a Workspace. A name that
// does not fit a DNS label is shortened with a hash of the full name.
func WorkspaceRunnerName(workspace string) string {
	return boundedName("ws-", workspace)
}

// ActorRunnerName is the actor runner name of an Actor. The UID makes it
// fresh for every Actor, because needy refuses to mint a name that a live
// runner still holds.
func ActorRunnerName(uid string) string {
	return "actor-" + strings.ReplaceAll(uid, "-", "")[:12]
}

func boundedName(prefix, name string) string {
	if len(prefix)+len(name) <= runnerNameMaxLength {
		return prefix + name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	keep := runnerNameMaxLength - len(prefix) - len(suffix)

	return prefix + strings.TrimRight(name[:keep], "-") + suffix
}

// StaticRunner is one entry of the daemon's runners file.
type StaticRunner struct {
	Name           string            `json:"name"`
	CredentialHash string            `json:"credential_hash"`
	Roles          []string          `json:"roles"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// StaticClient is one entry of the daemon's clients file.
type StaticClient struct {
	Name      string `json:"name"`
	Access    string `json:"access"`
	TokenHash string `json:"token_hash"`
}

// ClientsFile renders the daemon's clients file.
func ClientsFile(clients []StaticClient) ([]byte, error) {
	sorted := slices.Clone(clients)
	slices.SortFunc(sorted, func(a, b StaticClient) int {
		return strings.Compare(a.Name, b.Name)
	})
	if sorted == nil {
		sorted = []StaticClient{}
	}
	data, err := yaml.Marshal(map[string][]StaticClient{"clients": sorted})
	if err != nil {
		return nil, fmt.Errorf("render needy clients file: %w", err)
	}

	return data, nil
}

// RunnersFile renders the daemon's runners file. The output is sorted by name
// so that an unchanged set keeps the Secret unchanged.
func RunnersFile(runners []StaticRunner) ([]byte, error) {
	sorted := slices.Clone(runners)
	slices.SortFunc(sorted, func(a, b StaticRunner) int {
		return strings.Compare(a.Name, b.Name)
	})
	if sorted == nil {
		sorted = []StaticRunner{}
	}
	data, err := yaml.Marshal(map[string][]StaticRunner{"runners": sorted})
	if err != nil {
		return nil, fmt.Errorf("render needy runners file: %w", err)
	}

	return data, nil
}
