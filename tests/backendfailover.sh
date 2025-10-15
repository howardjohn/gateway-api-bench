#!/bin/bash
# set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

gateways=(agentgateway/agentgateway  envoy/envoy-gateway istio/istio nginx/nginx)

go run "${WD}/backendfailover" --gateways="$(join_by ',' "${gateways[@]}")" `log-flag` "$@"
