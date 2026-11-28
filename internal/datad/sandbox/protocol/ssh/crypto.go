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

// Package datadssh implements the in-Pod mTLS SSH Data Plane protocol backed
// by rckube.Supervisor, along with hardened SSH cryptographic helpers shared
// with the Data Plane Gateway.
package datadssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	// DefaultPort is the default in-Pod TCP port for the mTLS SSH Data Plane listener.
	DefaultPort = 2222
	// MinRSABits is the minimum accepted RSA modulus bit length for SSH keys.
	MinRSABits = 2048
)

// HardenedSSHConfig returns an ssh.Config restricted to modern, RFC-recommended
// key exchanges (including post-quantum hybrid ML-KEM-768 + X25519), AEAD
// ciphers, and Encrypt-then-MAC algorithms. Legacy SHA-1, CBC, and weak DH
// groups are excluded.
func HardenedSSHConfig() ssh.Config {
	return ssh.Config{
		KeyExchanges: []string{
			ssh.KeyExchangeMLKEM768X25519,
			ssh.KeyExchangeCurve25519,
			ssh.KeyExchangeECDHP256,
			ssh.KeyExchangeECDHP384,
		},
		Ciphers: []string{
			ssh.CipherChaCha20Poly1305,
			ssh.CipherAES256GCM,
			ssh.CipherAES128GCM,
		},
		MACs: []string{
			ssh.HMACSHA256ETM,
			ssh.HMACSHA512ETM,
		},
	}
}

// AllowedPublicKeyAlgorithms returns the set of modern public-key signature
// algorithms accepted for client authentication (excluding legacy SHA-1 ssh-rsa).
func AllowedPublicKeyAlgorithms() []string {
	return []string{
		ssh.KeyAlgoED25519,
		ssh.KeyAlgoSKED25519,
		ssh.KeyAlgoECDSA256,
		ssh.KeyAlgoECDSA384,
		ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKECDSA256,
		ssh.KeyAlgoRSASHA256,
		ssh.KeyAlgoRSASHA512,
	}
}

// ValidatePublicKey verifies that pub uses an allowed algorithm and meets
// minimum cryptographic strength requirements (for example RSA >= 2048 bits).
func ValidatePublicKey(pub ssh.PublicKey) error {
	if pub == nil {
		return errors.New("nil SSH public key")
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519,
		ssh.KeyAlgoSKED25519,
		ssh.KeyAlgoECDSA256,
		ssh.KeyAlgoECDSA384,
		ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKECDSA256:
		return nil
	case ssh.KeyAlgoRSA:
		cpk, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return errors.New("unable to inspect RSA public key size")
		}
		rsaPub, ok := cpk.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || rsaPub == nil {
			return errors.New("invalid RSA public key")
		}
		if rsaPub.N.BitLen() < MinRSABits {
			return fmt.Errorf("RSA key size %d bits is below minimum %d bits", rsaPub.N.BitLen(), MinRSABits)
		}
		return nil
	default:
		return fmt.Errorf("unsupported SSH public key type %q", pub.Type())
	}
}

// GenerateEd25519Signer generates a fresh Ed25519 SSH host signer using
// crypto/rand.Reader.
func GenerateEd25519Signer() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Ed25519 host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("create SSH signer from Ed25519 key: %w", err)
	}
	return signer, nil
}

// LoadOrGenerateHostKey loads a PEM-encoded SSH private key from path, or
// generates an ephemeral Ed25519 key when path is empty. The boolean reports
// whether an ephemeral key was generated.
func LoadOrGenerateHostKey(path string) (ssh.Signer, bool, error) {
	if strings.TrimSpace(path) == "" {
		signer, err := GenerateEd25519Signer()
		if err != nil {
			return nil, false, err
		}
		return signer, true, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read SSH host key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, false, fmt.Errorf("parse SSH host key %s: %w", path, err)
	}
	if err := ValidatePublicKey(signer.PublicKey()); err != nil {
		return nil, false, fmt.Errorf("validate SSH host key %s: %w", path, err)
	}
	return signer, false, nil
}

// ParseAuthorizedKeys strictly parses OpenSSH authorized_keys entries. Blank
// lines and '#' comments are ignored. Entries with unsupported OpenSSH options
// (such as from=, command=, restrict, no-port-forwarding), weak key sizes, or
// malformed lines return an error so restrictions are never silently stripped.
func ParseAuthorizedKeys(entries []string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for _, entry := range entries {
		for line := range strings.SplitSeq(entry, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			pub, _, options, _, err := ssh.ParseAuthorizedKey([]byte(trimmed))
			if err != nil {
				return nil, fmt.Errorf("parse authorized key: %w", err)
			}
			if len(options) > 0 {
				return nil, fmt.Errorf("authorized_keys options (%s) are not supported", strings.Join(options, ","))
			}
			if err := ValidatePublicKey(pub); err != nil {
				return nil, err
			}
			keys = append(keys, pub)
		}
	}
	return keys, nil
}

// ParseAuthorizedKeysLenient parses all valid, unrestricted, strong OpenSSH
// public keys from entries and skips malformed, weak, or option-bearing lines
// so one bad line in a shared Secret cannot lock out other valid keys while
// still failing closed on any restricted or weak entry.
func ParseAuthorizedKeysLenient(entries []string) []ssh.PublicKey {
	var keys []ssh.PublicKey
	for _, entry := range entries {
		for line := range strings.SplitSeq(entry, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			pub, _, options, _, err := ssh.ParseAuthorizedKey([]byte(trimmed))
			if err != nil || len(options) > 0 || ValidatePublicKey(pub) != nil {
				continue
			}
			keys = append(keys, pub)
		}
	}
	return keys
}

// MatchAuthorizedKey checks whether candidate matches any key in allowed using
// constant-time comparison across all keys without early exit.
func MatchAuthorizedKey(candidate ssh.PublicKey, allowed []ssh.PublicKey) bool {
	if candidate == nil || len(allowed) == 0 || ValidatePublicKey(candidate) != nil {
		return false
	}
	candBytes := candidate.Marshal()
	candType := candidate.Type()
	matched := 0
	for _, key := range allowed {
		if key == nil {
			continue
		}
		keyBytes := key.Marshal()
		eq := subtle.ConstantTimeCompare(candBytes, keyBytes)
		if key.Type() == candType {
			matched |= eq
		}
	}
	return matched == 1
}

// RestrictServerConfigPublicKeyAlgorithms configures the SSH server config to
// reject deprecated SHA-1 signature algorithms while retaining modern ones.
func RestrictServerConfigPublicKeyAlgorithms(cfg *ssh.ServerConfig) {
	cfg.PublicKeyAuthAlgorithms = slices.Clone(AllowedPublicKeyAlgorithms())
}

// ErrUnauthorized is returned when SSH client authentication fails.
var ErrUnauthorized = errors.New("ssh authentication failed")
