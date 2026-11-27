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
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/nekomeowww/rc/internal/datad/pki"
	"github.com/nekomeowww/rc/pkg/envd"
	"github.com/nekomeowww/rc/pkg/execd"
)

// Run executes the `manager data-plane-gateway` subcommand.
func Run(args []string, scheme *runtime.Scheme) error {
	fs := flag.NewFlagSet("data-plane-gateway", flag.ExitOnError)
	var listenAddr string
	var tlsCertFile string
	var tlsKeyFile string
	var caFile string
	var clientCertFile string
	var clientKeyFile string
	var caSecretNamespace string
	var caSecretName string
	var execdUpstreamPort int
	var upstreamPort int

	fs.StringVar(&listenAddr, "listen", ":8080", "Address the Data Plane Gateway listens on")
	fs.StringVar(&tlsCertFile, "tls-cert-file", "", "Optional TLS certificate file for external callers")
	fs.StringVar(&tlsKeyFile, "tls-key-file", "", "Optional TLS private key file for external callers")
	fs.StringVar(&caFile, "ca-file", "", "PEM CA certificate file for verifying Workspace runtime server certs")
	fs.StringVar(&clientCertFile, "client-cert-file", "", "PEM client certificate file for mTLS to Workspace runtimes")
	fs.StringVar(&clientKeyFile, "client-key-file", "", "PEM client private key file for mTLS to Workspace runtimes")
	fs.StringVar(&caSecretNamespace, "ca-secret-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace of the internal Data Plane CA Secret (used when --ca-file is unset)")
	fs.StringVar(&caSecretName, "ca-secret-name", "rc-data-plane-ca", "Name of the internal Data Plane CA Secret")
	fs.IntVar(&execdUpstreamPort, "execd-upstream-port", execd.DefaultPort,
		"TCP port of the primary execd listener on Workspace runtime Pods")
	fs.IntVar(&upstreamPort, "upstream-port", envd.DefaultPort, "TCP port of the envd listener on Workspace runtime Pods")

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
		certProvider = func(ctx context.Context) ([]byte, []byte, []byte, error) {
			if caSecretNamespace == "" {
				return nil, nil, nil, fmt.Errorf(
					"data plane gateway requires --ca-file/--client-cert-file/--client-key-file or --ca-secret-namespace",
				)
			}
			secret := &corev1.Secret{}
			key := types.NamespacedName{Namespace: caSecretNamespace, Name: caSecretName}
			if err := kubeClient.Get(ctx, key, secret); err != nil {
				return nil, nil, nil, err
			}
			bundle, err := pki.IssueClientCert(secret.Data["tls.crt"], secret.Data["tls.key"], "", time.Hour)
			if err != nil {
				return nil, nil, nil, err
			}
			return bundle.CAPEM, bundle.CertPEM, bundle.KeyPEM, nil
		}
	}

	resolver := &KubernetesResolver{
		Client:    kubeClient,
		ExecdPort: int32(execdUpstreamPort),
		Port:      int32(upstreamPort),
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

	ctx := ctrl.SetupSignalHandler()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Info("Starting Data Plane Gateway", "listen", listenAddr, "execdPort", execdUpstreamPort, "envdPort", upstreamPort)
	if tlsCertFile != "" && tlsKeyFile != "" {
		err = httpServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
	} else {
		err = httpServer.ListenAndServe()
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
