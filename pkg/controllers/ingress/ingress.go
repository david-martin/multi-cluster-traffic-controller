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

package ingress

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mctcv1 "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1"
	v1 "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/dns"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/traffic"
)

type HostService interface {
	RegisterHost(ctx context.Context, h string, id string, zone v1.DNSZone) (*mctcv1.DNSRecord, error)
	GetManagedZones() []dns.Zone
	EnsureManagedHost(ctx context.Context, t traffic.Interface) ([]string, []*mctcv1.DNSRecord, error)
	AddEndPoints(ctx context.Context, t traffic.Interface) error
	RemoveEndpoints(ctx context.Context, t traffic.Interface) error
}

type CertificateService interface {
	EnsureCertificate(ctx context.Context, host string, owner metav1.Object) error
	GetCertificateSecret(ctx context.Context, host string) (*corev1.Secret, error)
}

type DnsService interface {
	PatchTargets(ctx context.Context, targets, hosts []string, clusterID string, remove bool) error
}

// Ingress reconciles a Ingress object
type Ingress struct {
	client.Client
	Scheme             *runtime.Scheme
	ControlPlaneClient client.Client
	Host               string
	ClusterID          string
	Certificates       CertificateService
	DNS                DnsService
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.1/pkg/reconcile
func (r *Ingress) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	current := &networkingv1.Ingress{}
	err := r.Get(ctx, client.ObjectKey{Name: req.Name, Namespace: req.Namespace}, current)
	if err != nil {
		return ctrl.Result{}, err
	}
	target := current.DeepCopy()
	accessor := traffic.NewIngress(target)
	res, err := r.Handle(ctx, accessor)
	if err != nil {
		return res, err
	}
	if !equality.Semantic.DeepEqual(current, target) {
		log.Log.Info("update the ingress")
		r.Client.Update(ctx, target)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	return res, nil
}

func (r *Ingress) Handle(ctx context.Context, trafficAccessor traffic.Interface) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	log.Log.Info("got traffic object", "kind", trafficAccessor.GetKind(), "name", trafficAccessor.GetName(), "namespace", trafficAccessor.GetNamespace())
	if trafficAccessor.GetDeletionTimestamp() != nil && !trafficAccessor.GetDeletionTimestamp().IsZero() {
		err := r.DNS.PatchTargets(ctx, []string{}, trafficAccessor.GetHosts(), r.ClusterID, true)
		if err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, err
		}
		controllerutil.RemoveFinalizer(trafficAccessor, traffic.TrafficFinalizer)
		return ctrl.Result{}, nil
	}

	// verify host is correct
	// no managed host assigned, assign one
	// create empty DNSRecord with assigned host
	hosts := trafficAccessor.GetHosts()

	for _, host := range hosts {
		// when certificate ready copy secret (need to add event handler for certs)
		// only once certificate is ready update DNS based status of ingress
		secret, err := r.Certificates.GetCertificateSecret(ctx, host)
		if err != nil && !k8serrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// if err is not exists return and wait
		if err != nil {
			log.Log.Info("tls secret does not exist yet for host " + host + " requeue")
			return ctrl.Result{Requeue: true, RequeueAfter: time.Second * 10}, nil
		}
		log.Log.Info("certificate exists for host", "host", host)

		//copy secret
		if secret != nil {
			if err := r.copySecretToWorkloadCluster(ctx, trafficAccessor, secret, host); err != nil {
				return ctrl.Result{}, err
			}
			trafficAccessor.AddTLS(host, secret)
		}

		log.Log.Info("certificate secret in place for  host adding dns endpoints", "host", host)
	}

	//patch DNS on control plane
	dnsTargets, err := trafficAccessor.GetDNSTargets()
	if err != nil {
		return ctrl.Result{}, err
	}
	targets := []string{}
	for _, target := range dnsTargets {
		targets = append(targets, target.Value)
	}

	//adding DNS patches
	if len(targets) > 0 && trafficAccessor.GetDeletionTimestamp() == nil {
		err = r.DNS.PatchTargets(ctx, targets, trafficAccessor.GetHosts(), r.ClusterID, false)
		if err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, err
		}
		controllerutil.AddFinalizer(trafficAccessor, traffic.TrafficFinalizer)
	} else { //removing DNS patches
		err = r.DNS.PatchTargets(ctx, targets, trafficAccessor.GetHosts(), r.ClusterID, true)
		if err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: 10 * time.Second}, err
		}
		controllerutil.RemoveFinalizer(trafficAccessor, traffic.TrafficFinalizer)
	}
	return ctrl.Result{}, nil
}

func (r *Ingress) copySecretToWorkloadCluster(ctx context.Context, trafficAccessor traffic.Interface, tls *corev1.Secret, host string) error {
	log.Log.Info(fmt.Sprintf("tls secret ready for host %s. copying secret", host))
	copySecret := tls.DeepCopy()
	copySecret.ObjectMeta = metav1.ObjectMeta{
		Name:      host,
		Namespace: trafficAccessor.GetNamespace(),
	}
	if err := r.Client.Create(ctx, copySecret, &client.CreateOptions{}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(copySecret), copySecret); err != nil {
				return err
			}
			copySecret.Data = tls.Data
			if err := r.Client.Update(ctx, copySecret, &client.UpdateOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Ingress) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Complete(r)
}
