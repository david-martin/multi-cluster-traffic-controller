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

package gateway

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/controllers/ingress"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/dns"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/traffic"
	"github.com/lithammer/shortuuid/v4"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	ClusterSyncerAnnotation               = "clustersync.kuadrant.io"
	GatewayClusterLabelSelectorAnnotation = "kuadrant.io/gateway-cluster-label-selector"
)

// GatewayReconciler reconciles a Gateway object
type GatewayReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Certificates ingress.CertificateService
	Host         ingress.HostService
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Gateway object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.1/pkg/reconcile
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	previous := &gatewayv1beta1.Gateway{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, previous)
	if err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			log.Error(err, "Unable to fetch Gateway")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if previous.GetDeletionTimestamp() != nil && !previous.GetDeletionTimestamp().IsZero() {
		log.Info("Gateway is deleting", "gateway", previous)
		return ctrl.Result{}, nil
	}

	// Check if the class name is one of ours
	// TODO: If the gateway class is a supported class, but the GatewayClass resource doesn't exist,
	//       just create it anyways as we know we can support it.
	//       Con: Use case for an admin to only allow certain supported GatewayClasses to be used?
	gatewayClass := &gatewayv1beta1.GatewayClass{}
	err = r.Client.Get(ctx, client.ObjectKey{Name: string(previous.Spec.GatewayClassName)}, gatewayClass)
	if err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			log.Error(err, "Unable to fetch GatewayClass")
			return ctrl.Result{}, err
		}
		// Ignore as class can't be retrieved
		log.Info("GatewayClass not found", "gatewayclass", previous.Spec.GatewayClassName)
		return ctrl.Result{}, nil
	}

	gateway := previous.DeepCopy()
	acceptedCondition := metav1.Condition{
		LastTransitionTime: metav1.Now(),
		Message:            fmt.Sprintf("Handled by %s", ControllerName),
		Reason:             string(gatewayv1beta1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Type:               string(gatewayv1beta1.GatewayConditionAccepted),
		ObservedGeneration: previous.Generation,
	}

	clusters := selectClusters(*gateway)
	var programmedCondition metav1.Condition
	// Update initial Programmed status
	if len(clusters) > 0 {
		programmedCondition = metav1.Condition{
			LastTransitionTime: metav1.Now(),
			Message:            "Waiting for controller",
			Reason:             string(gatewayv1beta1.GatewayReasonPending),
			Status:             metav1.ConditionUnknown,
			Type:               string(gatewayv1beta1.GatewayConditionProgrammed),
			ObservedGeneration: previous.Generation,
		}
	} else {
		programmedCondition = metav1.Condition{
			LastTransitionTime: metav1.Now(),
			Message:            "No clusters match selection",
			Reason:             string(gatewayv1beta1.GatewayReasonPending),
			Status:             metav1.ConditionFalse,
			Type:               string(gatewayv1beta1.GatewayConditionProgrammed),
			ObservedGeneration: previous.Generation,
		}
	}
	// Save status conditions at this point
	gateway.Status.Conditions = []metav1.Condition{acceptedCondition, programmedCondition}
	if !reflect.DeepEqual(gateway.Status, previous.Status) {
		log.Info("Updating Gateway status", "gatewayStatus", gateway.Status)
		err = r.Status().Update(ctx, gateway)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Don't do anything else until at least 1 cluster matches.
	if len(clusters) > 0 {
		trafficAccessor := traffic.NewGateway(gateway)
		// TODO: Review potential states of reconcile and if the Gateway status should reflect different stages
		//       rather than ending the reconcile early without updating Gateway status
		hosts := trafficAccessor.GetHosts()

		log.Info("hosts", "hosts", hosts)
		for _, host := range hosts {
			// create certificate resource for assigned host
			if err := r.Certificates.EnsureCertificate(ctx, host, gateway); err != nil && !k8serrors.IsAlreadyExists(err) {
				log.Error(err, "Error ensuring certificate")
				return ctrl.Result{}, err
			}

			// Check if certificate secret is ready
			secret, err := r.Certificates.GetCertificateSecret(ctx, host)
			if err != nil && !k8serrors.IsNotFound(err) {
				log.Error(err, "Error getting certificate secret")
				return ctrl.Result{}, err
			}
			if err != nil {
				log.Info("tls secret does not exist yet for host " + host + " requeue")
				return ctrl.Result{Requeue: true, RequeueAfter: time.Second * 10}, nil
			}
			log.Info("certificate exists for host", "host", host)

			//sync secret to clusters
			if secret != nil {
				updatedSecret := secret.DeepCopy()
				applyClusterSyncerAnnotationsToObject(updatedSecret, clusters)
				if !reflect.DeepEqual(updatedSecret, secret) {
					log.Info("Updating Certificate secret annotations", "secret", secret.Name)
					err = r.Update(ctx, updatedSecret)
					if err != nil {
						return ctrl.Result{}, err
					}
				}
				trafficAccessor.AddTLS(host, secret)
			}

			log.Info("certificate secret in place for host. Adding dns endpoints", "host", host)
		}

		// TODO: When do we know when the certificate secret has synced?
		// TODO: Some listeners may not have a HTTPRoute yet in the data plane.
		//       Should those targets be omitted from the DNSRecord?
		// TODO: Move this logic into dns service

		zones := r.Host.GetManagedZones()
		hostKey := shortuuid.NewWithNamespace(trafficAccessor.GetNamespace() + trafficAccessor.GetName())
		for _, host := range hosts {
			var chosenZone dns.Zone
			// TODO: Validate hosts against managed zones before creating dns record & certificate.
			//       Custom hosts with certificates are OK and we'll skip these
			for _, z := range zones {
				if z.Default {
					chosenZone = z
					break
				}
			}
			if chosenZone.ID == "" {
				log.Info("No zone to use")
				// ignoring & moving on
			}
			// TODO: ownerRefs e.g.
			// err = controllerutil.SetControllerReference(parentZone, nsRecord, r.Scheme)
			record, err := r.Host.RegisterHost(ctx, host, hostKey, chosenZone.DNSZone)
			if err != nil {
				log.Error(err, "failed to register host ")
				return ctrl.Result{}, err
			}
			log.Info("Registered Host", "record", record)
		}

		err = r.Host.AddEndPoints(ctx, trafficAccessor)
		if err != nil {
			log.Error(err, "Error adding endpoints")
			return ctrl.Result{}, err
		}

		applyClusterSyncerAnnotationsToObject(gateway, clusters)
		if !reflect.DeepEqual(gateway, previous) {
			log.Info("Updating Gateway", "gateway", gateway)
			err = r.Update(ctx, gateway)
			if err != nil {
				return ctrl.Result{}, err
			}
		}

		// Update programmed condition
		programmedCondition = metav1.Condition{
			LastTransitionTime: metav1.Now(),
			Message:            fmt.Sprintf("Gateway configured in data plane cluster(s) - [%v]", strings.Join(clusters, ",")),
			Reason:             string(gatewayv1beta1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			Type:               string(gatewayv1beta1.GatewayConditionProgrammed),
			ObservedGeneration: previous.Generation,
		}
		// Update status conditions again
		gateway.Status.Conditions = []metav1.Condition{acceptedCondition, programmedCondition}
		if !reflect.DeepEqual(gateway.Status, previous.Status) {
			log.Info("Updating Gateway status", "gatewayStatus", gateway.Status)
			err = r.Status().Update(ctx, gateway)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

func applyClusterSyncerAnnotationsToObject(obj metav1.Object, clusters []string) {
	annotations := obj.GetAnnotations()
	if len(annotations) == 0 {
		annotations = map[string]string{}
	}
	for _, cluster := range clusters {
		annotations[fmt.Sprintf("%s/%s", ClusterSyncerAnnotation, cluster)] = "True"
	}
	obj.SetAnnotations(annotations)
}

func findConditionByType(conditions []metav1.Condition, conditionType gatewayv1beta1.GatewayConditionType) *metav1.Condition {
	for _, condition := range conditions {
		if condition.Type == string(conditionType) {
			return &condition
		}
	}
	return nil
}

func selectClusters(gateway gatewayv1beta1.Gateway) []string {
	if gateway.Annotations == nil {
		return []string{}
	}

	selector := gateway.Annotations[GatewayClusterLabelSelectorAnnotation]
	log.Log.Info("selectClusters", "selector", selector)

	// TODO: Lookup clusters and select based on gateway cluster label selector annotation
	// HARDCODED IMPLEMENTATION
	// Issue: https://github.com/Kuadrant/multi-cluster-traffic-controller/issues/52
	if selector == "type=test" {
		return []string{"test_cluster_one"}
	}
	return []string{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1beta1.Gateway{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			gateway := object.(*gatewayv1beta1.Gateway)
			return slice.ContainsString(getSupportedClasses(), string(gateway.Spec.GatewayClassName))
		})).
		Complete(r)
}
