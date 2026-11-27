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
// certificates used between the Data Plane Gateway and Workspace runtimes.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
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
)

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

// GenerateCA creates a new self-signed ECDSA P-256 certificate authority.
func GenerateCA(validity time.Duration) (Bundle, error) {
	if validity <= 0 {
		validity = DefaultCAValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Bundle{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "rc-data-plane-ca",
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return Bundle{}, fmt.Errorf("create CA certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Bundle{}, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

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

func issueLeaf(caCertPEM, caKeyPEM []byte, commonName string, dnsNames []string, extUsage x509.ExtKeyUsage, validity time.Duration) (Bundle, error) {
	if validity <= 0 {
		validity = DefaultLeafValidity
	}
	caPair, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		return Bundle{}, fmt.Errorf("parse CA keypair: %w", err)
	}
	caCert, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		return Bundle{}, fmt.Errorf("parse CA certificate: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Bundle{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: commonName,
		},
		DNSNames:    dnsNames,
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{extUsage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caPair.PrivateKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("create leaf certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Bundle{}, fmt.Errorf("marshal leaf key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return Bundle{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		CAPEM:   caCertPEM,
	}, nil
}

var defaultALPNProtos = []string{
	"h2",
	"http/1.1",
}

// ReloadingServerTLSConfig builds a server tls.Config that reloads the server
// certificate and client CA pool from disk on every handshake, so Secret
// rotation takes effect without restarting the Pod.
func ReloadingServerTLSConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	if _, err := loadServerTLS(caFile, certFile, keyFile); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		NextProtos: defaultALPNProtos,
		GetConfigForClient: func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
			return loadServerTLS(caFile, certFile, keyFile)
		},
	}, nil
}

func loadServerTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read data plane CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse data plane CA PEM: no certificates found")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load data plane server certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		Certificates: []tls.Certificate{cert},
		NextProtos:   defaultALPNProtos,
	}, nil
}

// ClientTLSConfig builds an mTLS client tls.Config for dialing a Workspace
// runtime with the expected ServerName.
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
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
		NextProtos:   defaultALPNProtos,
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
