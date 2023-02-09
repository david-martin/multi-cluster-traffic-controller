/*
Copyright 2022 The MultiCluster Traffic Controller Authors.
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
	"context"
	"flag"
	"os"

	certmanv1 "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/clusterSecret"
	kuadrantiov1 "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/controllers/ingress"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/dns"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/tls"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	defaultCtrlNS       = "argocd"
	defaultCertProvider = "glbc-ca"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(kuadrantiov1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var controlPlaneConfigSecretName string
	var controlPlaneConfigSecretNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8084", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8085", "The address the probe endpoint binds to.")
	flag.StringVar(&controlPlaneConfigSecretName, "control-plane-cluster", "control-plane-cluster", "The name of the secret with the control plane client configuration")
	flag.StringVar(&controlPlaneConfigSecretNamespace, "control-plane-config-namespace", "default", "The namespace containing the secret with the control plane client configuration")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "fb80029c-agent.kuadrant.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	//create the control plane client
	controlConfigSecret := &corev1.Secret{}
	err = mgr.GetClient().Get(context.Background(), client.ObjectKey{Name: controlPlaneConfigSecretName, Namespace: controlPlaneConfigSecretNamespace}, controlConfigSecret)
	if err != nil {
		setupLog.Error(err, "Syncer agent missing control plane config secret", "name", controlPlaneConfigSecretName, "namespace", controlPlaneConfigSecretNamespace)
		os.Exit(1)
	}
	controlClient, err := clusterSecret.ClientFromSecret(controlConfigSecret)
	if err != nil {
		setupLog.Error(err, "Syncer agent failed to create client from control plane config secret", "error", err)
		os.Exit(1)
	}
	//add expected custom resources to control plane client scheme
	err = kuadrantiov1.AddToScheme(controlClient.Scheme())
	if err != nil {
		setupLog.Error(err, "Syncer agent failed to add client scheme", "error", err)
		os.Exit(1)
	}

	err = certmanv1.AddToScheme(controlClient.Scheme())
	if err != nil {
		setupLog.Error(err, "Syncer agent failed to add client scheme", "error", err)
		os.Exit(1)
	}

	certificates := tls.NewService(controlClient, defaultCtrlNS, defaultCertProvider)
	dns := dns.NewService(controlClient, dns.NewSafeHostResolver(dns.NewDefaultHostResolver()), defaultCtrlNS)
	//start the ingress syncer with a traffic handler
	if err = (&ingress.Ingress{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Host:               mgr.GetConfig().Host,
		Certificates:       certificates,
		DNS:                dns,
		ControlPlaneClient: controlClient,
		ClusterID:          string(controlConfigSecret.Data["name"]),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Ingress")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
