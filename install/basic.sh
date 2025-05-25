#!/bin/bash

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)

kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.3.0/standard-install.yaml
# Required by Cilium or nothing works
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/gateway-api/v1.3.0/config/crd/experimental/gateway.networking.k8s.io_tlsroutes.yaml

helm upgrade --install --create-namespace --namespace envoy-gateway-system --version v1.4.0 eg oci://docker.io/envoyproxy/gateway-helm \
  --set deployment.envoyGateway.resources.limits.memory=null # disable limits to match other gateways

helm upgrade --install --create-namespace --namespace kgateway-system --version v2.0.1 kgateway-crds oci://cr.kgateway.dev/kgateway-dev/charts/kgateway-crds
helm upgrade --install --namespace kgateway-system --version v2.0.1 kgateway oci://cr.kgateway.dev/kgateway-dev/charts/kgateway

cat <<EOF | helm upgrade --install istiod --create-namespace --namespace istio-system --version 1.26.0 oci://gcr.io/istio-release/charts/istiod -f -
global:
  proxy:
    resources:
      limits: null # disable limits to match other gateways
autoscaling: # disable autoscaling for more consistent tests
  enabled: false
EOF

helm upgrade --install kong --namespace kong-system --create-namespace --version v0.19.0 --repo https://charts.konghq.com ingress

helm upgrade --install nginx --namespace nginx-system --create-namespace --version 1.6.2 oci://ghcr.io/nginx/charts/nginx-gateway-fabric


kubectl apply -f https://raw.githubusercontent.com/traefik/traefik/v3.4/docs/content/reference/dynamic-configuration/kubernetes-gateway-rbac.yml
cat <<EOF | helm upgrade --install traefik --namespace traefik-system --create-namespace --version v35.3.0 --repo https://helm.traefik.io/traefik traefik -f -
providers:
  kubernetesGateway:
    enabled: true
gateway:
  enabled: false
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
kubectl create namespace kgateway
kubectl create namespace kong
kubectl create namespace traefik
kubectl create namespace cilium
kubectl create namespace nginx
kubectl apply -f "${WD}/gateways.yaml"
