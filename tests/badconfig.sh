#!/bin/bash
# Bad config isolation: does one invalid route block valid routes?
#
# 1. Apply a valid route (good-before) and wait for it to work.
# 2. Apply an invalid route (bad). It passes API validation, but has two rules with things the spec allows yet
#    not every implementation can handle, so every implementation sees something invalid:
#    - a lookahead regex: RE2 (Envoy) and Go/Rust regex engines can't compile it (nginx's PCRE can)
#    - a '%' in a header value: Envoy treats it as a formatter and rejects it (nginx rejects it in status)
#    They are separate rules so an implementation that rejects one rule (e.g. validates the regex) still has
#    to process the other.
# 3. Apply another valid route (good-after). It must start working; if not, the bad route is blocking updates.
# 4. Remove the bad route and check good-after starts working (the gateway recovers).
#
# A route only counts as working if the response carries its x-route tag, so catch-all routes don't mask failures.
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)
source "$WD/common.sh"

TIMEOUT="${TIMEOUT:-5}"
NS=default

parent-refs() {
  echo "  parentRefs:"
  for gw in "${gateways[@]}"; do
    echo "  - name: ${gw#*/}"
    echo "    namespace: ${gw%/*}"
  done
}

# route <name> [extra lines for the rule] [extra rules]
route() {
  cat <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: badconfig-$1
  namespace: ${NS}
spec:
$(parent-refs)
  hostnames: [badconfig-$1.example.com]
  rules:
  - backendRefs: [{name: badconfig-backend, port: 80}]
$2
    filters:
    - type: ResponseHeaderModifier
      responseHeaderModifier:
        set:
        - {name: x-route, value: $1}
$3
EOF
}

bad_match='    matches:
    - path: {type: RegularExpression, value: "/(?=foo).*"}'
bad_rule='  - backendRefs: [{name: badconfig-backend, port: 80}]
    matches:
    - path: {type: PathPrefix, value: /}
    filters:
    - type: ResponseHeaderModifier
      responseHeaderModifier:
        set:
        - name: x-discount
          value: "100% off"'

works() {
  curl -s -o /dev/null -D - -m 2 -H "Host: badconfig-$1.example.com" "http://$2/" | grep -qi "^x-route: $1"
}

# wait-for <route> <ip>: succeeds once the route works, fails after TIMEOUT seconds
wait-for() {
  for _ in $(seq "$TIMEOUT"); do
    works "$1" "$2" && return 0
    sleep 1
  done
  return 1
}

backend() {
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: badconfig-backend
  namespace: ${NS}
spec:
  selector:
    matchLabels:
      app: badconfig-backend
  template:
    metadata:
      labels:
        app: badconfig-backend
    spec:
      containers:
      - name: backend
        image: howardjohn/hyper-server
---
apiVersion: v1
kind: Service
metadata:
  name: badconfig-backend
  namespace: ${NS}
spec:
  selector:
    app: badconfig-backend
  ports:
  - name: http
    port: 80
    targetPort: 8080
EOF
}

cleanup() {
  kubectl delete httproute -n "$NS" badconfig-good-before badconfig-good-after badconfig-bad --ignore-not-found >/dev/null
  backend | kubectl delete -f - --ignore-not-found >/dev/null
}
trap cleanup EXIT
cleanup

backend | kubectl apply -f - >/dev/null
kubectl rollout status -n "$NS" deploy/badconfig-backend --timeout=120s >/dev/null

declare -A ip
for gw in "${gateways[@]}"; do
  ip[$gw]=$(gw-address "$gw")
done

echo "Applying valid route (good-before)..."
route good-before | kubectl apply -f - >/dev/null
for gw in "${gateways[@]}"; do
  wait-for good-before "${ip[$gw]}" || echo "  warning: good-before never worked on $gw; results for it are meaningless"
done

echo "Applying invalid route (bad)..."
route bad "$bad_match" "$bad_rule" | kubectl apply -f - >/dev/null
sleep 5

echo "Applying valid route (good-after), waiting up to ${TIMEOUT}s..."
route good-after | kubectl apply -f - >/dev/null
declare -A after
for gw in "${gateways[@]}"; do
  wait-for good-after "${ip[$gw]}" && after[$gw]=ok || after[$gw]=BLOCKED
done
declare -A before
for gw in "${gateways[@]}"; do
  works good-before "${ip[$gw]}" && before[$gw]=ok || before[$gw]=BROKEN
done

echo "Removing invalid route, checking blocked gateways recover..."
kubectl delete httproute -n "$NS" badconfig-bad >/dev/null
declare -A recovered
for gw in "${gateways[@]}"; do
  [[ "${after[$gw]}" == ok ]] && continue
  wait-for good-after "${ip[$gw]}" && recovered[$gw]=yes || recovered[$gw]=NO
done

echo
for gw in "${gateways[@]}"; do
  if [[ "${before[$gw]}" == ok && "${after[$gw]}" == ok ]]; then verdict="✅"; else verdict="❌"; fi
  line=$(printf '%s %-28s existing route: %-7s new route: %-8s' "$verdict" "$gw" "${before[$gw]}" "${after[$gw]}")
  [[ -n "${recovered[$gw]}" ]] && line+="  recovers once bad route removed: ${recovered[$gw]}"
  echo "$line"
done
