#!/bin/bash

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

cat <<EOF | kubectl apply -f - --server-side=true
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
spec:
  selector:
    matchLabels:
      app: echo
  template:
    metadata:
      labels:
        app: echo
    spec:
      securityContext:
        sysctls:
        - name: net.ipv4.ip_unprivileged_port_start
          value: "0"
      containers:
      - name: echo
        image: gcr.io/istio-testing/app:latest
        args:
        - --port=80
---
apiVersion: v1
kind: Service
metadata:
  name: echo
spec:
  selector:
    app: echo
  ports:
  - name: http
    port: 80
EOF
kubectl rollout status deployment echo -n default --timeout=90s

for gw in "${gateways[@]}"; do
  name="$(<<<"$gw" cut -d/ -f 2)"
  namespace="$(<<<"$gw" cut -d/ -f 1)"
  cat <<EOF | kubectl apply -f - --server-side=true
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $name
  namespace: default
spec:
  parentRefs:
  - name: $name
    namespace: $namespace
  rules:
  - backendRefs:
    - name: echo
      port: 80
EOF
done

targets=()
for i in {0..1000}; do
  for gw in "${gateways[@]}"; do
    curl $(gw-address $gw)?delay=30s &
  done
  sleep .1
done
