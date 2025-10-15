#!/bin/bash
set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

gateways=(agentgateway/agentgateway  envoy/envoy-gateway istio/istio nginx/nginx)
mode="$1"
shift

if [[ "$mode" == "split" ]];then
  for gw in "${gateways[@]}"; do
    go run "${WD}/attachedroutes/attachedroutes.go" --gateways="$gw" `log-flag` "$@"
  done
else
  go run "${WD}/attachedroutes/attachedroutes.go" --gateways="$(join_by ',' "${gateways[@]}")" `log-flag` "$@"
fi
