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

package gateway

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/nekomeowww/rc/internal/datad/pki"
	datadssh "github.com/nekomeowww/rc/internal/datad/sandbox/protocol/ssh"
	"github.com/nekomeowww/rc/pkg/envd"
	"github.com/nekomeowww/rc/pkg/execd"
)

// Run executes the `manager data-plane-gateway` subcommand.
func Run(args []string, scheme *runtime.Scheme) error {
	fs := flag.NewFlagSet("data-plane-gateway", flag.ExitOnError)
	var listenAddr string
	var tlsCertFile string
	var tlsKeyFile string
	var sshListenAddr string
	var sshTLSListenAddr string
	var sshTLSCertFile string
	var sshTLSKeyFile string
	var sshHostKeyFile string
	var sshDefaultAuthority string
	var sshDisablePasswordAuth bool
	var sshAuthGRPCTarget string
	var sshAuthGRPCTimeout time.Duration
	var sshAuthGRPCCAFile string
	var caFile string
	var clientCertFile string
	var clientKeyFile string
	var caSecretNamespace string
	var caSecretName string
	var execdUpstreamPort int
	var upstreamPort int
	var sshUpstreamPort int

	fs.StringVar(&listenAddr, "listen", ":8080", "Address the Data Plane Gateway listens on")
	fs.StringVar(&tlsCertFile, "tls-cert-file", "", "Optional TLS certificate file for external callers")
	fs.StringVar(&tlsKeyFile, "tls-key-file", "", "Optional TLS private key file for external callers")
	fs.StringVar(&sshListenAddr, "ssh-listen", "", "Optional TCP address for the SSH Data Plane Gateway (for example :2222)")
	fs.StringVar(&sshTLSListenAddr, "ssh-tls-listen", "", "Optional TLS-wrapped SSH address for SNI subdomain routing (for example :2223)")
	fs.StringVar(&sshTLSCertFile, "ssh-tls-cert-file", "", "TLS certificate file for --ssh-tls-listen")
	fs.StringVar(&sshTLSKeyFile, "ssh-tls-key-file", "", "TLS private key file for --ssh-tls-listen")
	fs.StringVar(&sshHostKeyFile, "ssh-host-key-file", "", "Optional PEM SSH host private key file for the SSH Gateway (generates ephemeral Ed25519 key when unset)")
	fs.StringVar(&sshDefaultAuthority, "ssh-default-authority", "", "Optional default authority passed to SSH auth plugins when SNI is absent")
	fs.BoolVar(&sshDisablePasswordAuth, "ssh-disable-password-auth", false, "Disable SSH password authentication, requiring public keys")
	fs.StringVar(&sshAuthGRPCTarget, "ssh-auth-grpc-target", "", "Optional gRPC endpoint implementing rc.datad.ssh.v1alpha1.SSHAuthService for external SSH auth/routing")
	fs.DurationVar(&sshAuthGRPCTimeout, "ssh-auth-grpc-timeout", defaultGRPCAuthTimeout, "Per-RPC timeout for the external gRPC SSH auth plugin")
	fs.StringVar(&sshAuthGRPCCAFile, "ssh-auth-grpc-ca-file", "", "Optional CA PEM file for TLS to --ssh-auth-grpc-target (insecure when unset)")
	fs.StringVar(&caFile, "ca-file", "", "PEM CA certificate file for verifying Workspace runtime server certs")
	fs.StringVar(&clientCertFile, "client-cert-file", "", "PEM client certificate file for mTLS to Workspace runtimes")
	fs.StringVar(&clientKeyFile, "client-key-file", "", "PEM client private key file for mTLS to Workspace runtimes")
	fs.StringVar(&caSecretNamespace, "ca-secret-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace of the internal Data Plane CA Secret (used when --ca-file is unset)")
	fs.StringVar(&caSecretName, "ca-secret-name", "rc-data-plane-ca", "Name of the internal Data Plane CA Secret")
	fs.IntVar(&execdUpstreamPort, "execd-upstream-port", execd.DefaultPort,
		"TCP port of the primary execd listener on Workspace runtime Pods")
	fs.IntVar(&upstreamPort, "upstream-port", envd.DefaultPort, "TCP port of the envd listener on Workspace runtime Pods")
	fs.IntVar(&sshUpstreamPort, "ssh-upstream-port", datadssh.DefaultPort, "TCP port of the mTLS SSH listener on Workspace runtime Pods")

	opts := zap.Options{Development: true}
	opts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("data-plane-gateway")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes config: %w", err)
	}
	kubeClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	var certProvider ClientCertProvider
	if caFile != "" && clientCertFile != "" && clientKeyFile != "" {
		certProvider = func(_ context.Context) ([]byte, []byte, []byte, error) {
			caPEM, err := os.ReadFile(caFile)
			if err != nil {
				return nil, nil, nil, err
			}
			certPEM, err := os.ReadFile(clientCertFile)
			if err != nil {
				return nil, nil, nil, err
			}
			keyPEM, err := os.ReadFile(clientKeyFile)
			if err != nil {
				return nil, nil, nil, err
			}
			return caPEM, certPEM, keyPEM, nil
		}
	} else {
		var (
			certMu        sync.Mutex
			cachedBundle  pki.Bundle
			cachedExpires time.Time
		)
		certProvider = func(ctx context.Context) ([]byte, []byte, []byte, error) {
			if caSecretNamespace == "" {
				return nil, nil, nil, fmt.Errorf(
					"data plane gateway requires --ca-file/--client-cert-file/--client-key-file or --ca-secret-namespace",
				)
			}
			certMu.Lock()
			if len(cachedBundle.CertPEM) > 0 && time.Now().Before(cachedExpires) {
				b := cachedBundle
				certMu.Unlock()
				return b.CAPEM, b.CertPEM, b.KeyPEM, nil
			}
			certMu.Unlock()

			secret := &corev1.Secret{}
			key := types.NamespacedName{Namespace: caSecretNamespace, Name: caSecretName}
			if err := kubeClient.Get(ctx, key, secret); err != nil {
				return nil, nil, nil, err
			}
			const clientCertValidity = time.Hour
			bundle, err := pki.IssueClientCert(secret.Data["tls.crt"], secret.Data["tls.key"], "", clientCertValidity)
			if err != nil {
				return nil, nil, nil, err
			}
			certMu.Lock()
			cachedBundle = bundle
			cachedExpires = time.Now().Add(clientCertValidity - 5*time.Minute)
			certMu.Unlock()
			return bundle.CAPEM, bundle.CertPEM, bundle.KeyPEM, nil
		}
	}

	resolver := &KubernetesResolver{
		Client:    kubeClient,
		ExecdPort: int32(execdUpstreamPort),
		Port:      int32(upstreamPort),
		SSHPort:   int32(sshUpstreamPort),
	}
	gw := NewServer(resolver, certProvider)

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           gw.Handler(),
		Protocols:         protocols,
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()

	sshErrChs, sshCleanup, err := startSSHGateways(ctx, cancel, resolver, certProvider, sshGatewayOptions{
		listenAddr:          sshListenAddr,
		tlsListenAddr:       sshTLSListenAddr,
		tlsCertFile:         sshTLSCertFile,
		tlsKeyFile:          sshTLSKeyFile,
		hostKeyFile:         sshHostKeyFile,
		defaultAuthority:    sshDefaultAuthority,
		disablePasswordAuth: sshDisablePasswordAuth,
		authGRPCTarget:      sshAuthGRPCTarget,
		authGRPCTimeout:     sshAuthGRPCTimeout,
		authGRPCCAFile:      sshAuthGRPCCAFile,
		upstreamPort:        sshUpstreamPort,
	})
	if err != nil {
		return err
	}
	defer sshCleanup()

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Info("Starting Data Plane Gateway", "listen", listenAddr, "execdPort", execdUpstreamPort, "envdPort", upstreamPort)
	if tlsCertFile != "" && tlsKeyFile != "" {
		err = httpServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	} else {
		err = httpServer.ListenAndServe()
	}
	cancel()
	if err == http.ErrServerClosed {
		err = nil
	}
	for _, ch := range sshErrChs {
		if sshErr := <-ch; sshErr != nil && err == nil {
			err = sshErr
		}
	}
	return err
}

type sshGatewayOptions struct {
	listenAddr          string
	tlsListenAddr       string
	tlsCertFile         string
	tlsKeyFile          string
	hostKeyFile         string
	defaultAuthority    string
	disablePasswordAuth bool
	authGRPCTarget      string
	authGRPCTimeout     time.Duration
	authGRPCCAFile      string
	upstreamPort        int
}

func startSSHGateways(
	ctx context.Context,
	cancel context.CancelFunc,
	resolver Resolver,
	certProvider ClientCertProvider,
	opts sshGatewayOptions,
) ([]chan error, func(), error) {
	noop := func() {}
	if opts.listenAddr == "" && opts.tlsListenAddr == "" {
		return nil, noop, nil
	}
	log := ctrl.Log.WithName("data-plane-gateway")

	hostKey, ephemeral, err := datadssh.LoadOrGenerateHostKey(opts.hostKeyFile)
	if err != nil {
		return nil, noop, fmt.Errorf("load SSH gateway host key: %w", err)
	}
	if ephemeral {
		log.Info("Generated ephemeral Ed25519 SSH host key; pass --ssh-host-key-file for a persistent host key")
	}

	cleanup := noop
	var sshAuth SSHAuthenticator
	if opts.authGRPCTarget != "" {
		grpcAuth, grpcConn, dialErr := DialGRPCSSHAuthenticator(opts.authGRPCTarget, opts.authGRPCTimeout, opts.authGRPCCAFile)
		if dialErr != nil {
			return nil, noop, fmt.Errorf("configure gRPC SSH auth plugin: %w", dialErr)
		}
		cleanup = func() { _ = grpcConn.Close() }
		sshAuth = grpcAuth
		log.Info("Enabled external gRPC SSH auth plugin", "target", opts.authGRPCTarget, "timeout", opts.authGRPCTimeout)
	}

	sshGW, err := NewSSHServer(resolver, certProvider, SSHConfig{
		HostKey:             hostKey,
		Authenticator:       sshAuth,
		DefaultAuthority:    opts.defaultAuthority,
		DisablePasswordAuth: opts.disablePasswordAuth,
	})
	if err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("create SSH gateway: %w", err)
	}

	var errChs []chan error
	if opts.listenAddr != "" {
		ch := make(chan error, 1)
		errChs = append(errChs, ch)
		go func() {
			log.Info("Starting SSH Data Plane Gateway", "listen", opts.listenAddr, "upstreamPort", opts.upstreamPort)
			runErr := sshGW.Serve(ctx, opts.listenAddr)
			if runErr != nil {
				cancel()
			}
			ch <- runErr
		}()
	}

	if opts.tlsListenAddr != "" {
		if opts.tlsCertFile == "" || opts.tlsKeyFile == "" {
			cleanup()
			return nil, noop, fmt.Errorf("--ssh-tls-listen requires --ssh-tls-cert-file and --ssh-tls-key-file")
		}
		tlsCert, loadErr := tls.LoadX509KeyPair(opts.tlsCertFile, opts.tlsKeyFile)
		if loadErr != nil {
			cleanup()
			return nil, noop, fmt.Errorf("load SSH TLS key pair: %w", loadErr)
		}
		sshTLSCfg := &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{tlsCert},
			NextProtos:   []string{"ssh", pki.SSHProtocolALPN},
		}
		ch := make(chan error, 1)
		errChs = append(errChs, ch)
		go func() {
			log.Info("Starting TLS SNI SSH Data Plane Gateway", "listen", opts.tlsListenAddr, "upstreamPort", opts.upstreamPort)
			runErr := sshGW.ServeTLS(ctx, opts.tlsListenAddr, sshTLSCfg)
			if runErr != nil {
				cancel()
			}
			ch <- runErr
		}()
	}

	return errChs, cleanup, nil
}
