#!/bin/sh

# Istio ControlPlane
#kubectl --context kind-mctc-workload-1 -n istio-system create -f ./istiooperator.yaml

# Install MetalLB
kubectl --context kind-mctc-workload-1 apply -f https://raw.githubusercontent.com/metallb/metallb/v0.13.7/config/manifests/metallb-native.yaml
kubectl --context kind-mctc-workload-1 wait --namespace metallb-system --for=condition=ready pod --selector=app=metallb --timeout=90s
kubectl --context kind-mctc-workload-1 apply -f ./metallb.yaml


# TODO Delete nginx ingress controller & svc
# kubectl --context kind-mctc-workload-1 delete ingressclass nginx
# kubectl --context kind-mctc-workload-1 delete namespace ingress-nginx

# Update deployAddress in istio configmap
kubectl --context kind-mctc-workload-1 -n istio-system patch configmap istio -p '{"data": {"mesh": "defaultConfig:\n      discoveryAddress: istiod.istio-system.svc:15012\n      tracing:\n        zipkin:\n          address: zipkin.istio-system:9411\n    enablePrometheusMerge: true\n    rootNamespace: istio-system\n    trustDomain: cluster.local"}}'

# TODO delete istio pods to refresh config
kubectl --context kind-mctc-workload-1 -n istio-system delete $(kubectl --context kind-mctc-workload-1 -n istio-system get po -l app=istiod -o name)

# TODO Try host port 80 & 443 in extraportMappings as per https://www.danielstechblog.io/running-istio-on-kind-kubernetes-in-docker/

# Logs istiod
kubectl --context kind-mctc-workload-1 -n istio-system logs -f $(kubectl --context kind-mctc-workload-1 -n istio-system get po -l app=istiod -o name)

# Create Gateway Resource
kubectl --context kind-mctc-workload-1 apply -n istio-system -f ./gateway.yaml

# Logs istio
kubectl --context kind-mctc-workload-1 -n istio-system logs -f $(kubectl --context kind-mctc-workload-1 -n istio-system get po -l istio=ingressgateway -o name)


# Create sample application
kubectl --context kind-mctc-workload-1 label namespace default istio-injection=enabled
kubectl --context kind-mctc-workload-1 apply -n default -f ./echo.yaml

