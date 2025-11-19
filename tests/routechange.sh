#!/bin/bash
# set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

# Use the GATEWAYS environment variable. If unset, fall back to the defaults.
if [[ -n "${GATEWAYS}" ]]; then
  IFS=',' read -r -a gateways <<< "$GATEWAYS"
else
  gateways=(agentgateway/agentgateway  envoy/envoy-gateway istio/istio nginx/nginx)
fi

for gw in "${gateways[@]}"; do
  go run "${WD}/routechange" --gateways="$gw" "$@"
done