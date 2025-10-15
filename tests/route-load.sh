#!/bin/bash

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
          gateways:
          - agentgateway/agentgateway
          - envoy/envoy-gateway
          - istio/istio
          - nginx/nginx
nodes:
- name: node
  count: 20
EOF