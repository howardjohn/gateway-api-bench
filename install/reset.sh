#!/bin/bash

echo istio envoy kgateway kong traefik cilium nginx istio-system envoy-gateway-system kgateway-system kong-system traefik-system cilium-system nginx-system | \
  xargs -n1 -P 20 kubectl delete pods --all --force -n

kubectl delete pods -n kube-system -l app.kubernetes.io/part-of=cilium --force
kubectl delete pods -n monitoring -l app.kubernetes.io/part-of=prometheus --force
