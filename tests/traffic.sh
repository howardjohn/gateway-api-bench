#!/bin/bash

WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

gateways=(istio/istio kgateway/kgateway envoy/envoy-gateway cilium/cilium nginx/nginx traefik/traefik kong/kong)

cat <<EOF | kubectl apply -f - --server-side=true
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
  namespace: default
spec:
  selector:
    matchLabels:
      app: backend
  template:
    metadata:
      labels:
        app: backend
    spec:
      containers:
      - name: backend
        image: howardjohn/hyper-server
        resources:
          requests:
            memory: "64Mi"
            cpu: "100m"
---
apiVersion: v1
kind: Service
metadata:
  name: backend
  namespace: default
spec:
  selector:
    app: backend
  ports:
  - name: http
    port: 80
    targetPort: 8080
EOF
kubectl rollout status deployment backend -n default --timeout=90s

targets=()
for gw in "${gateways[@]}"; do
  name="$(<<<"$gw" cut -d/ -f 2)"
  namespace="$(<<<"$gw" cut -d/ -f 1)"
  targets+=("$(gw-address $gw)#$name")
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
    - name: backend
      port: 80
EOF
done

docker run --rm -v /tmp/results:/tmp/results --init -it --network=host howardjohn/benchtool "$@" "$(join_by , "${targets[@]}")"
