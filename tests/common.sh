
WD=$(dirname "$0")
WD=$(cd "$WD"; pwd)

# Use the GATEWAYS environment variable. If unset, fall back to the defaults.
if [[ -n "${GATEWAYS}" ]]; then
  IFS=',' read -r -a gateways <<< "$GATEWAYS"
else
  gateways=(agentgateway/agentgateway envoy/envoy-gateway istio/istio kgateway/kgateway nginx/nginx traefik/traefik)
fi

function gw-address() {
  local name;
  local namespace;
  if [[ "$1" == */* ]]; then
    namespace="$(<<<"$1" cut -d/ -f1)"
    name="$(<<<"$1" cut -d/ -f2)"
  elif [[ "${2:-}" != "" ]]; then
    namespace="$1"
    name="$2"
  else
    name="$1"
  fi

  kubectl get gateways.gateway.networking.k8s.io -ojsonpath='{.status.addresses[0].value}' "${namespace+--namespace=$namespace}" "$name"
}

function svc-address() {
  local name;
  local namespace;
  if [[ "$1" == *"/"* ]]; then
    namespace="$(<<<"$1" cut -d/ -f1)"
    name="$(<<<"$1" cut -d/ -f2)"
  elif [[ "${2:-}" != "" ]]; then
    namespace="$1"
    name="$2"
  else
    name="$1"
  fi

  kubectl  get service -o jsonpath='{.status.loadBalancer.ingress[0].ip}' ${namespace+--namespace=$namespace} "$name"
}

function join_by { local IFS="$1"; shift; echo "$*"; }

# Print the gateways as a YAML list, with each line prefixed by the given indent
function gateways-yaml() {
  local indent="$1"
  for gw in "${gateways[@]}"; do
    echo "${indent}- ${gw}"
  done
}

function log-flag() {
  local vic="$(svc-address monitoring victoria-logs 2> /dev/null || true)"
  echo "${vic:+--victoria=http://${vic}:9428}"
}