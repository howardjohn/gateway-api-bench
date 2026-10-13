#!/bin/bash

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)

kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/experimental-install.yaml --server-side

# Install only the Envoy Gateway CRDs; the chart bundles an older Gateway API version that conflicts with the one above
helm template eg-crds oci://docker.io/envoyproxy/gateway-crds-helm --version v1.9.3 \
  --set crds.gatewayAPI.enabled=false \
  --set crds.envoyGateway.enabled=true | kubectl apply --server-side -f -
helm upgrade --install --create-namespace --namespace envoy-gateway-system --version v1.9.3 eg oci://docker.io/envoyproxy/gateway-helm --skip-crds \
  --set config.envoyGateway.provider.kubernetes.deploy.type=GatewayNamespace \
  --set config.envoyGateway.admin.enablePprof=true \
  --set deployment.envoyGateway.resources.limits.memory=null # disable limits to match other gateways

helm upgrade -i --create-namespace --namespace agentgateway-system agentgateway-crds --version v1.7.0 oci://cr.agentgateway.dev/charts/agentgateway-crds
helm upgrade -i --namespace agentgateway-system agentgateway --version v1.7.0 oci://cr.agentgateway.dev/charts/agentgateway

helm upgrade -i --create-namespace --namespace kgateway-system kgateway-crds --version v2.5.0 oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds
helm upgrade -i --namespace kgateway-system kgateway --version v2.5.0 oci://cr.kgateway.dev/kgateway-dev/charts/kgateway

cat <<EOF | helm upgrade --install istiod --create-namespace --namespace istio-system --version 1.31.3 oci://ghcr.io/istio/release/charts/istiod -f -
global:
  proxy:
    resources:
      limits: null # disable limits to match other gateways
autoscaleEnabled: false # disable autoscaling for more consistent tests
env: # Needed for ListenerSet testing
  PILOT_ENABLE_ALPHA_GATEWAY_API: true
EOF

helm upgrade --install nginx --namespace nginx-system --create-namespace --version 2.8.0 oci://ghcr.io/nginx/charts/nginx-gateway-fabric

cat <<EOF | helm upgrade --install traefik --namespace traefik-system --create-namespace --version 42.2.0 --repo https://traefik.github.io/charts traefik -f -
providers:
  kubernetesGateway:
    enabled: true
gateway:
  enabled: false
api: # Expose the raw config and pprof on the traefik entrypoint (8080) for debugging
  insecure: true
  debug: true
ports:
  web:
    port: 80
podSecurityContext:
  sysctls:
  - name: net.ipv4.ip_unprivileged_port_start
    value: "0"
EOF

kubectl create namespace monitoring
kubectl apply -f "${WD}/prometheus.yaml"
kubectl apply -f "${WD}/grafana.yaml"
kubectl apply -f "${WD}/metrics-server.yaml"

kubectl create namespace istio
kubectl create namespace envoy
kubectl create namespace agentgateway
kubectl create namespace kgateway
kubectl create namespace nginx
kubectl create namespace traefik
kubectl apply -f "${WD}/gateways.yaml"
