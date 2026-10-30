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
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	vttestserveroperatoriov1alpha1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
	"github.com/likesavabutworse/vttestserver-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(vttestserveroperatoriov1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var datactlImage string
	var maxConcurrentReconciles int
	var watchNamespaces string
	var leaseDuration, renewDeadline, retryPeriod time.Duration
	var releaseLeaseOnShutdown bool
	var quiesceTimeout, uploadTimeout time.Duration
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager. "+
			"Run two or more replicas for high availability.")
	flag.DurationVar(&leaseDuration, "leader-election-lease-duration", 15*time.Second,
		"How long a leader lease stays valid. "+
			"After a crash of the leader, a new leader starts working after this time at most.")
	flag.BoolVar(&releaseLeaseOnShutdown, "leader-election-release-on-shutdown", true,
		"Give up the leader lease when the process gets SIGTERM, so the other replica takes over at once. "+
			"Turn it off only to test how a crash behaves: the other replica then waits for the lease to run out.")
	flag.DurationVar(&renewDeadline, "leader-election-renew-deadline", 10*time.Second,
		"How long the leader tries to renew the lease before it steps down. Must be shorter than the lease duration.")
	flag.DurationVar(&retryPeriod, "leader-election-retry-period", 2*time.Second,
		"How often a replica tries to take or renew the lease.")
	flag.DurationVar(&quiesceTimeout, "snapshot-quiesce-timeout", 10*time.Minute,
		"How long a snapshot waits for its server to stop before it fails and releases the server.")
	flag.DurationVar(&uploadTimeout, "snapshot-upload-timeout", 30*time.Minute,
		"How long the snapshot upload Job may run before it fails and the server is released.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.StringVar(&datactlImage, "datactl-image", "vttestserver-operator-datactl:latest",
		"The datactl image for restore init containers and snapshot Jobs.")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 3,
		"How many objects each controller reconciles at the same time. Objects are never reconciled twice at once.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma separated namespaces to watch. Empty means all namespaces. Needs RBAC in each of them.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server")
	// JSON logs at info level by default. Use --zap-devel for console logs at debug level.
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Disabled by default: HTTP/2 Stream Cancellation and Rapid Reset
	// (GHSA-qppj-fm5r-hxr3, GHSA-4374-p667-p6c8).
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// The chart creates the RBAC that authorizes scraping this (dist/chart/templates/rbac).
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// Without a cert path, controller-runtime generates a self-signed certificate for the metrics
	// server. That is fine for development; use cert-manager and --metrics-cert-path for production.
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
		HealthProbeBindAddress: probeAddr,
		Cache:                  cache.Options{DefaultNamespaces: namespaceConfig(watchNamespaces)},
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "5ad0790a.vttestserver-operator.io",
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
		// The program ends right after the manager stops, so a leader that gets SIGTERM (a rollout, a node
		// drain) gives up the lease at once and the other replica does not wait for the lease to run out.
		LeaderElectionReleaseOnCancel: releaseLeaseOnShutdown,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	metrics.Registry.MustRegister(controller.NewStateCollector(mgr.GetClient()))

	if err := (&controller.VTTestServerReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		DatactlImage:            datactlImage,
		MaxConcurrentReconciles: maxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "vttestserver")
		os.Exit(1)
	}
	if err := (&controller.VTTestServerSnapshotReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		DatactlImage:            datactlImage,
		MaxConcurrentReconciles: maxConcurrentReconciles,
		QuiesceTimeout:          quiesceTimeout,
		UploadTimeout:           uploadTimeout,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "vttestserversnapshot")
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

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// namespaceConfig turns the value of --watch-namespaces into cache settings. It returns nil, which means
// all namespaces, for an empty value.
func namespaceConfig(csv string) map[string]cache.Config {
	var out map[string]cache.Config
	for ns := range strings.SplitSeq(csv, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			if out == nil {
				out = map[string]cache.Config{}
			}
			out[ns] = cache.Config{}
		}
	}
	return out
}
