#!/bin/bash
set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

gateways=(istio/istio kgateway/kgateway envoy/envoy-gateway cilium/cilium nginx/nginx traefik/traefik kong/kong)
safe_gateways=(istio/istio kgateway/kgateway envoy/envoy-gateway cilium/cilium nginx/nginx)
mode="$1"
shift

if [[ "$mode" == "split" ]];then
  for gw in "${gateways[@]}"; do
    go run "${WD}/probe/probe.go" --gateways="$gw" `log-flag` "$@"
  done
else
  go run "${WD}/probe/probe.go" --gateways="$(join_by ',' "${safe_gateways[@]}")" `log-flag` "$@"
fi
