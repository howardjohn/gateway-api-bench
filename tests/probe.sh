#!/bin/bash
set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"


if [[ "$mode" == "split" ]];then
  for gw in "${gateways[@]}"; do
    go run "${WD}/probe/probe.go" --gateways="$gw" `log-flag` "$@"
  done
else
  go run "${WD}/probe/probe.go" --gateways="$(join_by ',' "${gateways[@]}")" `log-flag` "$@"
fi
