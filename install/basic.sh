#!/bin/bash

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)

kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml --server-side

helm upgrade --install --create-namespace --namespace envoy-gateway-system --version v1.5.3 eg oci://docker.io/envoyproxy/gateway-helm \
  --set config.envoyGateway.provider.kubernetes.deploy.type=GatewayNamespace \
  --set deployment.envoyGateway.resources.limits.memory=null # disable limits to match other gateways

helm upgrade --install --create-namespace --namespace kgateway-system --version v2.2.0-alpha.1 kgateway-crds oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds
# Enable Alpha APIs for ListenerSet testing
helm upgrade --install --namespace kgateway-system --version v2.2.0-alpha.1 kgateway oci://cr.kgateway.dev/kgateway-dev/charts/kgateway \
  --set agentgateway.enabled=true \
  --set agentgateway.enableAlphaAPIs=true \ 
  --set envoy.enabled=false

cat <<EOF | helm upgrade --install istiod --create-namespace --namespace istio-system --version 1.27.1 oci://gcr.io/istio-release/charts/istiod -f -
global:
  proxy:
    resources:
      limits: null # disable limits to match other gateways
autoscaleEnabled: false # disable autoscaling for more consistent tests
env: # Needed for ListenerSet testing
  PILOT_ENABLE_ALPHA_GATEWAY_API: true
EOF

helm upgrade --install nginx --namespace nginx-system --create-namespace --version 2.1.4 oci://ghcr.io/nginx/charts/nginx-gateway-fabric

kubectl create namespace monitoring
kubectl apply -f "${WD}/prometheus.yaml"
kubectl apply -f "${WD}/grafana.yaml"
kubectl apply -f "${WD}/metrics-server.yaml"

kubectl create namespace istio
kubectl create namespace envoy
kubectl create namespace agentgateway
kubectl create namespace nginx
kubectl apply -f "${WD}/gateways.yaml"
