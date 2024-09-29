#!/bin/sh
set -e

echo "deleting existing manifests"
rm -rf ./dev/minio-operator.yaml
rm -rf ./dev/minio-tenant.yaml

echo "generating manifests"

kubectl create namespace minio-operator \
    --dry-run=client \
    --output=yaml \
    > ./dev/minio-operator.yaml
helm template \
    --create-namespace \
    --namespace minio-operator \
    --repo=https://operator.min.io \
    operator operator >> ./dev/minio-operator.yaml
kubectl create namespace minio-tenant \
    --dry-run=client \
    --output=yaml \
    > ./dev/minio-tenant.yaml
helm template \
    --create-namespace \
    --namespace minio-tenant \
    --repo=https://operator.min.io \
    --set="tenant.pools[0].name=default" \
    --set="tenant.pools[0].servers=1" \
    --set="tenant.pools[0].volumesPerServer=1" \
    --set="tenant.pools[0].size=100Mi" \
    tenant tenant >> ./dev/minio-tenant.yaml