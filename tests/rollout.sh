#!/bin/bash
# Rolling restart the backend (--target=backend, default) or each gateway's data plane (--target=gateway)
# while sending traffic, and report any downtime.
# set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

for gw in "${gateways[@]}"; do
  go run "${WD}/rollout" --gateways="$gw" "$@"
done
