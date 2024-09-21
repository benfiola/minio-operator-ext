#!/bin/sh 
set -e

for command in minikube helm kubectl mc kubefwd; do
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

echo "deploy openldap"
kubectl apply -f ./dev/openldap.yaml

echo "deploy custom resources"
kubectl apply -f ./manifests/crds.yaml

echo "deploy test resources"
kubectl apply -f ./manifests/example-resources.yaml

code="1"
set +e
while [ "${code}" != "0" ]; do
  echo "wait for minio tenant to be ready"
  kubectl wait --namespace minio-tenant --selector v1.min.io/tenant=myminio --for=condition=ready pod --timeout=0s > /dev/null 2>&1
  code="$?"
  sleep 5
done
set -e
echo "minio tenant ready"

echo "forwarding ports"
./dev/forward-ports.sh

code="1"
set +e
while [ "${code}" != "0" ]; do
  echo "attempting to configure mc client"
  mc alias set local https://minio.minio-tenant.svc minio minio123  > /dev/null 2>&1
  code="$?"
  sleep 5;
done;
set -e
echo "mc client configured"