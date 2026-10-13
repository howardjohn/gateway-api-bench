#!/bin/bash
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

namespaces="${1:-10}"
routes="${2:-100}"

cat <<EOF | pilot-load cluster --config -
jitter:
  workloads: "2s"
  config: "1s"
gracePeriod: 500ms
namespaces:
  - name: mesh
    replicas: ${namespaces}
    applications:
    - name: app
      replicas: ${routes}
      pods: 1
      type: plain
      configs:
      - name: httproute
        config:
          hostnames: 3
          gateways:
$(gateways-yaml "          ")
nodes:
- name: node
  count: 60
EOF