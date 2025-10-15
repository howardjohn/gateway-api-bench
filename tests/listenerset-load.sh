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
    configs:
    - name: tls-secret
      config:
        Name: ns-cert
    applications:
    - name: app
      replicas: ${routes}
      pods: 1
      type: plain
      configs:
      - name: route
        config:
          gateways:
          - agentgateway/agentgateway
          # - envoy/envoy-gateway
          - istio/istio
          # - nginx/nginx
          routes: 16
      - name: listenerset
        config:
          gateways:
          - agentgateway/agentgateway
          # - envoy/envoy-gateway
          - istio/istio
          # - nginx/nginx
nodes:
- name: node
  count: 20
templates:
  route: |
    #refresh=true
    {{ range \$rc := until (int .routes) }}
    apiVersion: gateway.networking.k8s.io/v1
    kind: HTTPRoute
    metadata:
      name: {{\$.Name}}-{{.}}
    spec:
      hostnames:
        - {{\$.Name}}.example.com
      parentRefs:
      {{ range \$gw := \$.gateways }}
      {{ \$spl := split "/" \$gw }}
      - name: {{\$.Name}}-{{\$spl._1}}
        kind: XListenerSet
        group: gateway.networking.x-k8s.io
      {{ end }}
      rules:
        - backendRefs:
            - name: {{\$.Name}}
              port: 80
          matches:
            - path:
                type: PathPrefix
                value: /{{.}}/{{\$.RandNumber}}
    ---
    {{ end }}
  listenerset: |
    #refresh=false
    {{ range \$gw := .gateways }}
    {{ \$spl := split "/" \$gw }}
    apiVersion: gateway.networking.x-k8s.io/v1alpha1
    kind: XListenerSet
    metadata:
      name: {{\$.Name}}-{{\$spl._1}}
    spec:
      parentRef:
        name: {{\$spl._1}}
        namespace: {{\$spl._0}}
        kind: Gateway
        group: gateway.networking.k8s.io
      listeners:
        - name: {{\$.Name}}
          hostname: {{\$.Name}}.example.com
          protocol: HTTPS
          port: 443
          tls:
            mode: Terminate
            certificateRefs:
              - kind: Secret
                group: ""
                name: ns-cert
    ---
    {{ end }}
EOF