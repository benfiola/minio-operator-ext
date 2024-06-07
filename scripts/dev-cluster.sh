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