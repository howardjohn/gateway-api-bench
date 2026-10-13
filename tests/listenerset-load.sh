#!/bin/bash
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

# Traefik does not program ListenerSets (they stay Accepted=Unknown/Pending), so leave it out of this test
gateways=("${gateways[@]/traefik\/traefik}")
read -r -a gateways <<< "${gateways[*]}"

namespaces="${1:-10}"
routes="${2:-100}"

cat <<EOF | pilot-load cluster --config -
jitter:
  workloads: "2s"
  config: "1s"
gracePeriod: 100ms
namespaces:
  - name: mesh
    replicas: ${namespaces}
    applications:
    - name: tenant
      replicas: ${routes}
      pods: 1
      type: plain
      configs:
      - name: route
        count: 16
        config:
          gateways:
$(gateways-yaml "          ")
      - name: listenerset
        config:
          gateways:
$(gateways-yaml "          ")
      - name: cert
        config:
          hosts: 16
nodes:
- name: node
  count: 20
templates:
  cert: |
    #refresh=true
    {{ \$names := list }}
    {{ range \$i := until (int .hosts) }}{{ \$names = append \$names (printf "h-%d.%s.%s.example.com" \$i \$.Name \$.Namespace) }}{{ end }}
    {{ \$c := genSelfSignedCertWithKey (first \$names) nil \$names 3650 (genPrivateKey "ecdsa") }}
    apiVersion: v1
    kind: Secret
    metadata:
      name: {{.Name}}-cert
    type: kubernetes.io/tls
    data:
      tls.crt: {{ \$c.Cert | b64enc }}
      tls.key: {{ \$c.Key | b64enc }}
  route: |
    #refresh=true
    apiVersion: gateway.networking.k8s.io/v1
    kind: HTTPRoute
    metadata:
      name: {{.Name}}-{{.Index}}
    spec:
      hostnames:
        - h-{{.Index}}.{{.Name}}.{{.Namespace}}.example.com
      parentRefs:
      {{ range \$gw := .gateways }}
      {{ \$spl := split "/" \$gw }}
      - name: {{\$.Name}}-{{\$spl._1}}
        kind: ListenerSet
        group: gateway.networking.k8s.io
      {{ end }}
      rules:
        - backendRefs:
            - name: {{.Name}}
              port: 80
          matches:
            - path:
                type: PathPrefix
                value: /{{.Index}}/{{.RandNumber}}
  listenerset: |
    #refresh=false
    {{ range \$gw := .gateways }}
    {{ \$spl := split "/" \$gw }}
    apiVersion: gateway.networking.k8s.io/v1
    kind: ListenerSet
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
          hostname: "*.{{\$.Name}}.{{\$.Namespace}}.example.com"
          protocol: HTTPS
          port: 443
          tls:
            mode: Terminate
            certificateRefs:
              - kind: Secret
                group: ""
                name: {{\$.Name}}-cert
    ---
    {{ end }}
EOF
