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

// Package pki manages the internal mTLS certificate authority and leaf
// certificates used between the Data Plane Gateway and Workspace runtimes,
// built on go.step.sm/crypto and controller-runtime's certwatcher.
package pki

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"go.step.sm/crypto/keyutil"
	"go.step.sm/crypto/minica"
	"go.step.sm/crypto/pemutil"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

const (
	// WorkspaceServerDomain is the DNS suffix for Workspace Data Plane server certificates.
	WorkspaceServerDomain = "workspace.rc.ayaka.io"
	// GatewayClientCommonName is the CN of the Data Plane Gateway mTLS client certificate.
	GatewayClientCommonName = "data-plane-gateway.rc.ayaka.io"
	// DefaultCAValidity is the lifetime of an internally generated Data Plane CA.
	DefaultCAValidity = 365 * 24 * time.Hour
	// DefaultLeafValidity is the lifetime of internally issued leaf certificates.
	DefaultLeafValidity = 30 * 24 * time.Hour
	// SSHProtocolALPN is the dedicated TLS ALPN identifier for the mTLS SSH Data Plane.
	SSHProtocolALPN = "rc-ssh-v1"
)

var defaultALPNProtos = []string{
	"h2",
	"http/1.1",
}

// Bundle holds PEM-encoded certificate, private key, and CA certificate material.
type Bundle struct {
	CertPEM []byte
	KeyPEM  []byte
	CAPEM   []byte
}

// WorkspaceServerName returns the canonical TLS ServerName for a Workspace Data Plane.
func WorkspaceServerName(namespace, name string) string {
	return fmt.Sprintf("%s.%s.%s", name, namespace, WorkspaceServerDomain)
}

// GenerateCA creates a new self-signed certificate authority using go.step.sm/crypto/minica.
func GenerateCA(_ time.Duration) (Bundle, error) {
	ca, err := minica.New(minica.WithName("rc-data-plane-ca"))
	if err != nil {
		return Bundle{}, fmt.Errorf("generate CA: %w", err)
	}
	certBlock, err := pemutil.Serialize(ca.Root)
	if err != nil {
		return Bundle{}, fmt.Errorf("serialize CA certificate: %w", err)
	}
	keyBlock, err := pemutil.Serialize(ca.RootSigner)
	if err != nil {
		return Bundle{}, fmt.Errorf("serialize CA key: %w", err)
	}
	certPEM := pem.EncodeToMemory(certBlock)
	keyPEM := pem.EncodeToMemory(keyBlock)

	return Bundle{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		CAPEM:   certPEM,
	}, nil
}

// IssueServerCert signs a Workspace Data Plane server certificate with the given CA.
func IssueServerCert(caCertPEM, caKeyPEM []byte, serverName string, validity time.Duration) (Bundle, error) {
	return issueLeaf(caCertPEM, caKeyPEM, serverName, []string{serverName}, x509.ExtKeyUsageServerAuth, validity)
}

// IssueClientCert signs a Data Plane Gateway client certificate with the given CA.
func IssueClientCert(caCertPEM, caKeyPEM []byte, commonName string, validity time.Duration) (Bundle, error) {
	if commonName == "" {
		commonName = GatewayClientCommonName
	}
	return issueLeaf(caCertPEM, caKeyPEM, commonName, nil, x509.ExtKeyUsageClientAuth, validity)
}

func issueLeaf(
	caCertPEM, caKeyPEM []byte,
	commonName string,
	dnsNames []string,
	extUsage x509.ExtKeyUsage,
	validity time.Duration,
) (Bundle, error) {
	if validity <= 0 {
		validity = DefaultLeafValidity
	}
	caCert, err := pemutil.ParseCertificate(caCertPEM)
	if err != nil {
		return Bundle{}, fmt.Errorf("parse CA certificate: %w", err)
	}
	rawKey, err := pemutil.Parse(caKeyPEM)
	if err != nil {
		return Bundle{}, fmt.Errorf("parse CA key: %w", err)
	}
	caSigner, ok := rawKey.(crypto.Signer)
	if !ok {
		return Bundle{}, errors.New("CA private key does not implement crypto.Signer")
	}

	leafSigner, err := keyutil.GenerateDefaultSigner()
	if err != nil {
		return Bundle{}, fmt.Errorf("generate leaf key: %w", err)
	}
	now := time.Now().UTC()
	ca := &minica.CA{
		Intermediate: caCert,
		Signer:       caSigner,
	}
	cert, err := ca.Sign(&x509.Certificate{
		Subject:     pkix.Name{CommonName: commonName},
		DNSNames:    dnsNames,
		PublicKey:   leafSigner.Public(),
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{extUsage},
	})
	if err != nil {
		return Bundle{}, fmt.Errorf("sign leaf certificate: %w", err)
	}

	certBlock, err := pemutil.Serialize(cert)
	if err != nil {
		return Bundle{}, fmt.Errorf("serialize leaf certificate: %w", err)
	}
	keyBlock, err := pemutil.Serialize(leafSigner)
	if err != nil {
		return Bundle{}, fmt.Errorf("serialize leaf key: %w", err)
	}

	return Bundle{
		CertPEM: pem.EncodeToMemory(certBlock),
		KeyPEM:  pem.EncodeToMemory(keyBlock),
		CAPEM:   caCertPEM,
	}, nil
}

// ReloadingServerTLSConfig builds a server tls.Config backed by controller-runtime's
// CertWatcher (watching certFile and keyFile for rotation) and verifying client
// certificates against caFile.
func ReloadingServerTLSConfig(ctx context.Context, caFile, certFile, keyFile string) (*tls.Config, error) {
	return ReloadingServerTLSConfigWithALPN(ctx, caFile, certFile, keyFile, defaultALPNProtos)
}

// ReloadingServerTLSConfigWithALPN builds a TLS 1.3 mTLS server tls.Config
// with custom ALPN protocols for protocol separation.
func ReloadingServerTLSConfigWithALPN(ctx context.Context, caFile, certFile, keyFile string, nextProtos []string) (*tls.Config, error) {
	caPool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("create certificate watcher: %w", err)
	}
	go func() {
		_ = watcher.Start(ctx)
	}()

	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      caPool,
		GetCertificate: watcher.GetCertificate,
		NextProtos:     nextProtos,
	}, nil
}

func loadCAPool(caFile string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read data plane CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse data plane CA PEM: no certificates found")
	}
	return pool, nil
}

// ClientTLSConfig builds an mTLS client tls.Config for dialing a Workspace
// runtime with the expected ServerName.
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	return ClientTLSConfigWithALPN(caPEM, certPEM, keyPEM, serverName, defaultALPNProtos)
}

// ClientTLSConfigWithALPN builds a TLS 1.3 mTLS client tls.Config with custom
// ALPN protocols for dialing a Workspace runtime.
func ClientTLSConfigWithALPN(caPEM, certPEM, keyPEM []byte, serverName string, nextProtos []string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse data plane CA PEM: no certificates found")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse gateway client certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   serverName,
		NextProtos:   nextProtos,
	}, nil
}
