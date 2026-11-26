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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	repositoriesv1alpha1 "github.com/nekomeowww/rc/api/repositories/v1alpha1"
	configsv1alpha1 "github.com/nekomeowww/rc/api/v1alpha1"
	workspacesv1alpha1 "github.com/nekomeowww/rc/api/workspaces/v1alpha1"
	"github.com/nekomeowww/rc/internal/controller"
	repositoriescontroller "github.com/nekomeowww/rc/internal/controller/repositories"
	workspacescontroller "github.com/nekomeowww/rc/internal/controller/workspaces"
	processruntime "github.com/nekomeowww/rc/internal/execution"
	"github.com/nekomeowww/rc/internal/needy"
	"github.com/nekomeowww/rc/internal/version"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(configsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(repositoriesv1alpha1.AddToScheme(scheme))
	utilruntime.Must(workspacesv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string

	var runnerImage string
	var windowsRunnerImage string
	var darwinRunnerImage string
	var darwinWorkspaceRoot string
	var lxcfsDir string
	var rcKubeImage string
	var needyImage string
	var needyKVMResource string
	var needyKVMNodeSelector string
	var needyKVMTolerations string
	var needyKVMGroup int64
	var needyDaemonKVM bool
	var needyDaemonStorageClass string
	var needyGatewayBlockCIDRs string
	var needyControllerNamespace string

	var secureMetrics bool
	var enableHTTP2 bool

	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&runnerImage, "runner-image", "ghcr.io/nekomeowww/rc/runner:latest",
		"The image used by Repository and Worktree Jobs and blank Workspace runtimes.")
	flag.StringVar(&windowsRunnerImage, "windows-runner-image", "",
		"Default Windows Workspace image, used when spec.image is omitted.")
	flag.StringVar(&darwinRunnerImage, "darwin-runner-image", "",
		"Default macOS-vz Workspace VM image, used when spec.image is omitted.")
	flag.StringVar(&darwinWorkspaceRoot, "darwin-workspace-root", "",
		"Absolute host directory root shared into persistent Darwin Workspaces.")
	flag.StringVar(&lxcfsDir, "lxcfs-dir", "",
		"Host directory where LXCFS serves its proc files, for example /var/lib/lxcfs. "+
			"Linux Workspace runtime Pods bind-mount these files. Empty disables LXCFS.")
	flag.StringVar(&rcKubeImage, "rc-kube-image", version.DefaultRCKubeImage(),
		"The image that provides rc-kube and tini to Linux Workspace and WorkspaceEnvironment runtime Pods. "+
			"Empty selects the default, the official image of this controller version.")
	flag.StringVar(&needyImage, "needy-image", "",
		"The image that provides needy at /usr/local/bin/needy, a shell, and CA roots. "+
			"It runs the per-namespace needy daemon and Actor runners, and Workspaces copy needy from it. "+
			"Empty disables Actors and spec.needy.")
	flag.StringVar(&needyKVMResource, "needy-kvm-resource", "",
		"The extended resource that exposes /dev/kvm to Actor runner Pods, for example devices.kubevirt.io/kvm.")
	flag.StringVar(&needyKVMNodeSelector, "needy-kvm-node-selector", "",
		"Node selector of Pods that need KVM, as key=value pairs separated by commas.")
	flag.StringVar(&needyKVMTolerations, "needy-kvm-tolerations", "",
		"Tolerations of Pods that need KVM, as key[=value]:Effect entries separated by commas.")
	flag.Int64Var(&needyKVMGroup, "needy-kvm-group", 0,
		"The group that owns /dev/kvm on KVM nodes. Zero adds no supplemental group.")
	flag.BoolVar(&needyDaemonKVM, "needy-daemon-kvm", false,
		"Run each needy daemon on a KVM node with the KVM device, so it can build image steps itself.")
	flag.StringVar(&needyDaemonStorageClass, "needy-daemon-storage-class", "",
		"Storage class of each needy daemon's state volume. Empty keeps the state in an emptyDir, "+
			"which loses run history and paired clients when the daemon restarts.")
	flag.StringVar(&needyGatewayBlockCIDRs, "needy-gateway-block-cidrs", "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
		"Address ranges the needy actor gateway refuses, separated by commas: the cluster's Pod, Service, and node ranges.")
	flag.StringVar(&needyControllerNamespace, "needy-controller-namespace", os.Getenv("POD_NAMESPACE"),
		"The manager's namespace, whose manager Pods each needy daemon's NetworkPolicy admits. "+
			"Defaults to POD_NAMESPACE.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "9a17169b.rc.ayaka.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}
	podExecutor, err := processruntime.NewKubernetesPodExecutor(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "Failed to create process Pod executor")
		os.Exit(1)
	}
	processRuntime := processruntime.NewKubeRuntime(podExecutor)
	needyConfig, err := needyConfiguration(needyImage, needyKVMResource, needyKVMNodeSelector, needyKVMTolerations,
		needyKVMGroup, needyDaemonKVM, needyDaemonStorageClass, needyGatewayBlockCIDRs)
	if err != nil {
		setupLog.Error(err, "Invalid needy configuration")
		os.Exit(1)
	}
	needyConfig.ControllerNamespace = needyControllerNamespace

	if err := (&controller.AgentCredentialReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "agentcredential")
		os.Exit(1)
	}
	if err := (&controller.CredentialReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "credential")
		os.Exit(1)
	}
	if err := (&repositoriescontroller.RepositoryReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		RunnerImage: runnerImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repositories-repository")
		os.Exit(1)
	}
	if err := (&repositoriescontroller.RepositoryExecReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		RunnerImage: runnerImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repositories-repositoryexec")
		os.Exit(1)
	}
	if err := (&repositoriescontroller.WorktreeReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		RunnerImage: runnerImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repositories-worktree")
		os.Exit(1)
	}
	if err := (&workspacescontroller.WorkspaceEnvironmentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-workspaceenvironment")
		os.Exit(1)
	}
	if err := (&workspacescontroller.WorkspaceReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		RunnerImage:         runnerImage,
		WindowsRunnerImage:  windowsRunnerImage,
		DarwinRunnerImage:   darwinRunnerImage,
		DarwinWorkspaceRoot: darwinWorkspaceRoot,
		LXCFSDir:            lxcfsDir,
		RCKubeImage:         rcKubeImage,
		NeedyImage:          needyConfig.Image,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-workspace")
		os.Exit(1)
	}
	if err := (&workspacescontroller.WorkspaceRetentionReconciler{
		Client:   mgr.GetClient(),
		History:  processRuntime,
		Activity: processRuntime,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-workspace-retention")
		os.Exit(1)
	}
	if err := (&workspacescontroller.WorkspaceEnvironmentRetentionReconciler{
		Client:  mgr.GetClient(),
		History: processRuntime,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-environment-retention")
		os.Exit(1)
	}

	if err := (&workspacescontroller.WorkspaceExecReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Runtime:     processRuntime,
		RCKubeImage: rcKubeImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-workspaceexec")
		os.Exit(1)
	}
	if err := (&repositoriescontroller.WorktreeExecReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		RunnerImage: runnerImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repositories-worktreeexec")
		os.Exit(1)
	}
	if err := (&repositoriescontroller.RepositorySyncReconciler{
		RunnerImage: runnerImage,
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repositories-repositorysync")
		os.Exit(1)
	}
	if err := (&workspacescontroller.ActorReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: needyConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-actor")
		os.Exit(1)
	}
	if err := (&workspacescontroller.NeedyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: needyConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workspaces-needy")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager", "version", version.Version, "rcKubeImage", rcKubeImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// needyConfiguration validates the needy flags.
func needyConfiguration(image, kvmResource, nodeSelector, tolerations string, kvmGroup int64,
	daemonKVM bool, storageClass, blockCIDRs string) (needy.Config, error) {
	selector, err := needy.ParseSelector(nodeSelector)
	if err != nil {
		return needy.Config{}, err
	}
	parsedTolerations, err := needy.ParseTolerations(tolerations)
	if err != nil {
		return needy.Config{}, err
	}
	config := needy.Config{
		Image:              image,
		KVMResource:        corev1.ResourceName(kvmResource),
		KVMNodeSelector:    selector,
		KVMTolerations:     parsedTolerations,
		KVMGroup:           kvmGroup,
		DaemonKVM:          daemonKVM,
		DaemonStorageClass: storageClass,
	}
	for cidr := range strings.SplitSeq(blockCIDRs, ",") {
		if cidr = strings.TrimSpace(cidr); cidr != "" {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return needy.Config{}, fmt.Errorf("--needy-gateway-block-cidrs: %w", err)
			}
			config.GatewayBlockCIDRs = append(config.GatewayBlockCIDRs, cidr)
		}
	}
	if config.DaemonKVM && config.KVMResource == "" {
		return needy.Config{}, fmt.Errorf("--needy-daemon-kvm requires --needy-kvm-resource")
	}

	return config, nil
}
