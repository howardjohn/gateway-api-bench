#!/bin/bash
set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

# Set SKIP_SLOW=true to drop implementations that take too long to converge (Traefik)
if [[ "${SKIP_SLOW}" == "true" ]]; then
  gateways=("${gateways[@]/traefik\/traefik}")
  read -r -a gateways <<< "${gateways[*]}"
fi

if [[ "$mode" == "split" ]];then
  for gw in "${gateways[@]}"; do
    go run "${WD}/probe/probe.go" --gateways="$gw" `log-flag` "$@"
  done
else
  go run "${WD}/probe/probe.go" --gateways="$(join_by ',' "${gateways[@]}")" `log-flag` "$@"
fi
