#!/bin/bash
set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

gateways=(istio/istio kgateway/kgateway envoy/envoy-gateway cilium/cilium nginx/nginx traefik/traefik kong/kong)
safe_gateways=(istio/istio kgateway/kgateway traefik/traefik kong/kong)

for gw in "${safe_gateways[@]}"; do
  go run "${WD}/routechange" --gateways="$gw" "$@"
done