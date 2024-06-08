#!/bin/sh 
set -e

for command in minikube helm kubectl; do
    if ! command -v "${command}" > /dev/null; then
        2>&1 echo "error: ${command} not installed"
        exit 1
    fi
done

if [ ! -d "/workspaces/minio-operator-ext" ]; then
  2>&1 echo "error: must be run from devcontainer"
  exit 1
fi

echo "delete minikube cluster if exists"
minikube delete || true

echo "create minikube cluster"
minikube start --force

echo "install minio operator"
helm install \
  --namespace minio-operator \
  --create-namespace \
  --repo=https://operator.min.io \
  operator operator

echo "create minio tenant"
helm install \
  --namespace minio-tenant \
  --create-namespace \
  --repo=https://operator.min.io \
  tenant tenant

echo "deploy custom resources"
kubectl apply -f ./manifests/crds.yaml

echo "deploy test resources"
kubectl apply -f ./manifests/example_resources.yaml

echo "adding /etc/hosts entry for minio tenant service"
if [ ! -f "/etc/hosts.bak" ]; then
  cp /etc/hosts /etc/hosts.bak
fi
cp /etc/hosts.bak /etc/hosts
echo "127.0.0.1 minio.minio-tenant.svc.cluster.local" >> /etc/hosts

echo "waiting for minio tenant to be ready"
while true; do
  set +e
  kubectl wait --namespace minio-tenant --selector v1.min.io/tenant=myminio --for=condition=ready pod --timeout=0s > /dev/null 2>&1
  code="$?"
  set -e
  if [ "${code}" = "0" ]; then
    echo "minio tenant ready"
    break
  fi
  echo "minio tenant not ready"
  sleep 5
done

echo "forwarding minio tenant service port"
kubectl port-forward --namespace minio-tenant services/minio --pod-running-timeout=1h 443:443
