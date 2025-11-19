#!/bin/bash
# set -e
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

go run "${WD}/backendfailover" --gateways="$(join_by ',' "${gateways[@]}")" `log-flag` "$@"
