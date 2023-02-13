package traffic

import (
	"context"

	kuadrantv1 "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	TrafficFinalizer = "kuadrant.io/traffic-management"
)

type CreateOrUpdateTraffic func(ctx context.Context, i Interface) error
type DeleteTraffic func(ctx context.Context, i Interface) error

type Interface interface {
	runtime.Object
	metav1.Object
	AddManagedHost(h string) error
	GetKind() string
	GetHosts() []string
	GetCacheKey() string
	GetNamespaceName() types.NamespacedName
	AddTLS(host string, secret *corev1.Secret)
	HasTLS() bool
	GetTLS() []TLSConfig
	RemoveTLS(host []string)
	GetSpec() interface{}
	GetDNSTargets() ([]kuadrantv1.Target, error)
	GetWebhookConfigurations(host string, caBundle []byte) ([]*admissionv1.ValidatingWebhookConfiguration, []*admissionv1.MutatingWebhookConfiguration)
	ExposesOwnController() bool
}

type TLSConfig struct {
	Hosts      []string
	SecretName string
}

type Pending struct {
	Rules []networkingv1.IngressRule `json:"rules"`
}
