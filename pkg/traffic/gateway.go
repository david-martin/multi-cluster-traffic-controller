package traffic

import (
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/strings/slices"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
	kuadrantv1 "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1"
)

func NewGateway(g *gatewayv1beta1.Gateway) Interface {
	return &Gateway{Gateway: g}
}

type Gateway struct {
	*gatewayv1beta1.Gateway
}

func (a *Gateway) GetKind() string {
	return "Gateway"
}

func (a *Gateway) GetHosts() []string {
	var hosts []string
	for _, listener := range a.Spec.Listeners {
		host := (*string)(listener.Hostname)
		if !slices.Contains(hosts, *host) {
			hosts = append(hosts, *host)
		}
	}

	return hosts
}

func (a *Gateway) AddManagedHost(h string) error {
	// Not implemented for Gateways
	return nil
}

func (a *Gateway) HasTLS() bool {
	hasTLS := false
	for _, listener := range a.Spec.Listeners {
		if listener.TLS != nil {
			hasTLS = true
			break
		}
	}
	return hasTLS
}

func (a *Gateway) GetTLS() []TLSConfig {
	tls := []TLSConfig{}

	for _, listener := range a.Spec.Listeners {
		if listener.TLS != nil {
			tls = append(tls, TLSConfig{
				// TODO: Allow for 0 or multiple hosts in 1 listener
				Hosts: []string{fmt.Sprint(listener.Hostname)},
				// TODO: Allow for 0 or multiple certificate refs
				SecretName: string(listener.TLS.CertificateRefs[0].Name),
			})
		}
	}

	return tls
}

func (a *Gateway) AddTLS(host string, secret *corev1.Secret) {
	listeners := []gatewayv1beta1.Listener{}

	gatewayNS := gatewayv1beta1.Namespace(a.Namespace)
	for _, listener := range a.Spec.Listeners {
		if *(*string)(listener.Hostname) == host {
			listener.TLS = &gatewayv1beta1.GatewayTLSConfig{
				CertificateRefs: []gatewayv1beta1.SecretObjectReference{
					{
						Name:      gatewayv1beta1.ObjectName(secret.Name),
						Namespace: &gatewayNS,
					},
				},
			}
		}
		listeners = append(listeners, listener)
	}

	a.Spec.Listeners = listeners
}

func (a *Gateway) RemoveTLS(hosts []string) {
	for _, listener := range a.Spec.Listeners {
		if slice.ContainsString(hosts, fmt.Sprint(listener.Hostname)) {
			listener.TLS = nil
		}
	}
}

func (a *Gateway) GetSpec() interface{} {
	return a.Spec
}

func (a *Gateway) GetNamespaceName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: a.Namespace,
		Name:      a.Name,
	}
}

func (a *Gateway) GetCacheKey() string {
	key, _ := cache.MetaNamespaceKeyFunc(a)
	return key
}

func (a *Gateway) String() string {
	return fmt.Sprintf("kind: %v, namespace/name: %v", a.GetKind(), a.GetNamespaceName())
}

// GetDNSTargets will return the LB hosts and or IPs from the the Ingress object associated with the cluster they came from
func (a *Gateway) GetDNSTargets() ([]kuadrantv1.Target, error) {
	dnsTargets := []kuadrantv1.Target{}

	// TODO: Fetch Gateway IP Adresses from aggregated Gateway status
	// HARDCODED
	dnsTarget := kuadrantv1.Target{
		TargetType: kuadrantv1.TargetTypeIP,
		Value:      "172.18.0.1",
	}

	dnsTargets = append(dnsTargets, dnsTarget)

	return dnsTargets, nil
}

func (a *Gateway) GetWebhookConfigurations(host string, caBundle []byte) ([]*admissionv1.ValidatingWebhookConfiguration, []*admissionv1.MutatingWebhookConfiguration) {
	// Not implemented for Gateways
	return []*admissionv1.ValidatingWebhookConfiguration{}, []*admissionv1.MutatingWebhookConfiguration{}
}

func (a *Gateway) ExposesOwnController() bool {
	return false
}
