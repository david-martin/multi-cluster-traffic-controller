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

	corev1 "k8s.io/api/core/v1"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
	v1alpha1 "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/traffic"
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

type HostService interface {
	CreateDNSRecord(ctx context.Context, hostKey string, managedZone *v1alpha1.ManagedZone) (*v1alpha1.DNSRecord, error)
	GetManagedZone(ctx context.Context, t traffic.Interface) (*v1alpha1.ManagedZone, error)
	AddEndPoints(ctx context.Context, t traffic.Interface) error
	RemoveEndpoints(ctx context.Context, t traffic.Interface) error
}

type CertificateService interface {
	EnsureCertificate(ctx context.Context, host string, owner metav1.Object) error
	GetCertificateSecret(ctx context.Context, host string) (*corev1.Secret, error)
}

type GatewayHelper interface {
	GetGatewayStatuses() []gatewayv1beta1.GatewayStatus
	GetListenerNameByHost(host string) string
	GetListenerStatusesByListenerName(listenerName string) []gatewayv1beta1.ListenerStatus
}

// GatewayReconciler reconciles a Gateway object
type GatewayReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Certificates CertificateService
	Host         HostService
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update

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
		// TODO: Do we need to remove dns records and/or endpoints?
		//       Will ownerRefs be sufficient
		log.Info("Gateway is deleting", "gateway", previous.Name, "namespace", previous.Namespace)
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
	acceptedStatus := metav1.ConditionTrue
	programmedStatus, clusters, requeue, reconcileErr := r.reconcileGateway(ctx, *previous, gateway)

	// Update gateway spec/metadata
	if !reflect.DeepEqual(gateway, previous) {
		log.Info("Updating Gateway", "gateway", gateway.Name, "namespace", gateway.Namespace)
		err = r.Update(ctx, gateway)
		if err != nil {
			log.Error(err, "Error updating Gateway")
		}
	}

	// Update status
	gateway.Status.Conditions = buildStatusConditions(gateway.Status, previous.Generation, clusters, acceptedStatus, programmedStatus)
	if !reflect.DeepEqual(gateway.Status, previous.Status) {
		log.Info("Updating Gateway status", "gateway", gateway.Name, "namespace", gateway.Namespace)
		err = r.Status().Update(ctx, gateway)
		if err != nil {
			log.Error(err, "Error updating Gateway status")
		}
	}

	if requeue {
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second * 10}, reconcileErr
	}
	return ctrl.Result{}, reconcileErr
}

// Configures Gateway tls & dns for each cluster it targets.
// Returns the programmed status, a list of clusters that were programmed, if the gateway should be requeued, and any error
func (r *GatewayReconciler) reconcileGateway(ctx context.Context, previous gatewayv1beta1.Gateway, gateway *gatewayv1beta1.Gateway) (metav1.ConditionStatus, []string, bool, error) {
	log := log.FromContext(ctx)

	clusters := selectClusters(*gateway)
	// Don't do anything else until at least 1 cluster matches.
	if len(clusters) == 0 {
		// TODO: Handle any cleanup if there were previously clusters
		return metav1.ConditionFalse, clusters, false, nil
	}
	trafficAccessor := traffic.NewGateway(gateway)
	hosts := trafficAccessor.GetHosts()

	for _, host := range hosts {
		// create certificate resource for assigned host
		if err := r.Certificates.EnsureCertificate(ctx, host, gateway); err != nil && !k8serrors.IsAlreadyExists(err) {
			log.Error(err, "Error ensuring certificate")
			return metav1.ConditionUnknown, clusters, false, err
		}

		// Check if certificate secret is ready
		secret, err := r.Certificates.GetCertificateSecret(ctx, host)
		if err != nil && !k8serrors.IsNotFound(err) {
			log.Error(err, "Error getting certificate secret")
			return metav1.ConditionUnknown, clusters, false, err
		}
		if err != nil {
			log.Info("tls secret does not exist yet for host " + host + " requeue")
			return metav1.ConditionUnknown, clusters, true, err
		}
		log.Info("certificate exists for host", "host", host)

		//sync secret to clusters
		if secret != nil {
			updatedSecret := secret.DeepCopy()
			syncObjectToAllClusters(updatedSecret)
			if !reflect.DeepEqual(updatedSecret, secret) {
				log.Info("Updating Certificate secret annotations", "secret", secret.Name)
				err = r.Update(ctx, updatedSecret)
				if err != nil {
					return metav1.ConditionUnknown, clusters, false, err
				}
			}
			trafficAccessor.AddTLS(host, secret)
		}
		// Secrets don't have a status, so we can't say for sure if it's synced OK. Optimism here.
		log.Info("certificate secret in place for host. Adding dns endpoints", "host", host)
	}

	// TODO: Move this logic into dns service?
	// hostKey := shortuuid.NewWithNamespace(trafficAccessor.GetNamespace() + trafficAccessor.GetName())
	hasAnyAttachedRoutes := false
	for _, host := range hosts {
		// Only consider host for dns if there's at least 1 attached route to the listener in *any* gateway
		gatewayHelper := trafficAccessor.(GatewayHelper)
		listenerName := gatewayHelper.GetListenerNameByHost(host)
		listenerStatuses := gatewayHelper.GetListenerStatusesByListenerName(listenerName)
		hasAttachedRoutes := false
		for _, listenerStatus := range listenerStatuses {
			if listenerStatus.AttachedRoutes > 0 {
				hasAttachedRoutes = true
				break
			}
		}
		log.Info("hasAttachedRoutes", "host", host, "listenerName", listenerName, "hasAttachedRoutes", hasAttachedRoutes)

		if hasAttachedRoutes {
			hasAnyAttachedRoutes = true
			managedZone, err := r.Host.GetManagedZone(ctx, trafficAccessor)
			if err != nil {
				return metav1.ConditionUnknown, clusters, false, err
			}
			if managedZone == nil {
				log.Info("no managed zone available to use")
				continue
			}
			// TODO: ownerRefs e.g.
			// err = controllerutil.SetControllerReference(parentZone, nsRecord, r.Scheme)
			record, err := r.Host.CreateDNSRecord(ctx, host, managedZone)
			if err != nil {
				log.Error(err, "failed to register host ")
				return metav1.ConditionUnknown, clusters, false, err
			}
			log.Info("Registered Host", "record", record)
		}
	}
	syncObjectToAllClusters(gateway)

	if !hasAnyAttachedRoutes {
		log.Info("no hosts have any attached routes in any gateway yet")
		return metav1.ConditionUnknown, clusters, true, nil
	}

	// TODO: Do we need to check attachedRoutes again for each dns record, or is the logic above sufficient
	//       to ensure a record won't be created too early
	err := r.Host.AddEndPoints(ctx, trafficAccessor)
	if err != nil {
		log.Error(err, "Error adding endpoints")
		return metav1.ConditionUnknown, clusters, false, err
	}

	return metav1.ConditionTrue, clusters, false, nil
}

func buildStatusConditions(gatewayStatus gatewayv1beta1.GatewayStatus, generation int64, clusters []string, acceptedStatus metav1.ConditionStatus, programmedStatus metav1.ConditionStatus) []metav1.Condition {
	acceptedCondition := getConditionByType(gatewayStatus.Conditions, gatewayv1beta1.GatewayConditionAccepted)
	if (acceptedCondition == nil) || (acceptedCondition.Status != acceptedStatus) || (acceptedCondition.ObservedGeneration != generation) {
		// State has changed
		acceptedCondition = &metav1.Condition{
			LastTransitionTime: metav1.Now(),
			Message:            fmt.Sprintf("Handled by %s", ControllerName),
			Reason:             string(gatewayv1beta1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Type:               string(gatewayv1beta1.GatewayConditionAccepted),
			ObservedGeneration: generation,
		}
	}

	programmedCondition := getConditionByType(gatewayStatus.Conditions, gatewayv1beta1.GatewayConditionProgrammed)
	if (programmedCondition == nil) || (programmedCondition.Status != programmedStatus) || (programmedCondition.ObservedGeneration != generation) {
		// State has changed
		if programmedStatus == metav1.ConditionFalse {
			programmedCondition = &metav1.Condition{
				LastTransitionTime: metav1.Now(),
				Message:            "No clusters match selection",
				Reason:             string(gatewayv1beta1.GatewayReasonPending),
				Status:             metav1.ConditionFalse,
				Type:               string(gatewayv1beta1.GatewayConditionProgrammed),
				ObservedGeneration: generation,
			}
		} else if programmedStatus == metav1.ConditionTrue {
			programmedCondition = &metav1.Condition{
				LastTransitionTime: metav1.Now(),
				Message:            fmt.Sprintf("Gateway configured in data plane cluster(s) - [%v]", strings.Join(clusters, ",")),
				Reason:             string(gatewayv1beta1.GatewayConditionProgrammed),
				Status:             metav1.ConditionTrue,
				Type:               string(gatewayv1beta1.GatewayConditionProgrammed),
				ObservedGeneration: generation,
			}
		} else {
			// assume condition unknown i.e. programming is pending
			programmedCondition = &metav1.Condition{
				LastTransitionTime: metav1.Now(),
				Message:            "Waiting for controller",
				Reason:             string(gatewayv1beta1.GatewayReasonPending),
				Status:             metav1.ConditionUnknown,
				Type:               string(gatewayv1beta1.GatewayConditionProgrammed),
				ObservedGeneration: generation,
			}
		}
	}
	return []metav1.Condition{*acceptedCondition, *programmedCondition}
}

// func syncObjectToClusters(obj metav1.Object, clusters []string) {
// 	annotations := obj.GetAnnotations()
// 	if len(annotations) == 0 {
// 		annotations = map[string]string{}
// 	}
// 	for _, cluster := range clusters {
// 		annotations[fmt.Sprintf("%s/%s", syncer.MCTC_SYNC_ANNOTATION_PREFIX, cluster)] = "true"
// 	}
// 	obj.SetAnnotations(annotations)
// }

// TODO: Remove. This is a hack to enable simple 'all' placement of a resource
//
//		 in lieu of cluster representation in control plane.
//	     Use the above commented function instead.
func syncObjectToAllClusters(obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if len(annotations) == 0 {
		annotations = map[string]string{}
	}
	annotations[fmt.Sprintf("%s%s", syncer.MCTC_SYNC_ANNOTATION_PREFIX, syncer.MCTC_SYNC_ANNOTATION_WILDCARD)] = "true"
	obj.SetAnnotations(annotations)
}

func getConditionByType(conditions []metav1.Condition, conditionType gatewayv1beta1.GatewayConditionType) *metav1.Condition {
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
