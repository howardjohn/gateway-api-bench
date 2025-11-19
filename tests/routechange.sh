#!/bin/bash
# set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

for gw in "${gateways[@]}"; do
  go run "${WD}/routechange" --gateways="$gw" "$@"
done
