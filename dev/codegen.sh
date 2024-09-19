#!/bin/bash
set -e

PKG="github.com/benfiola/minio-operator-ext"
CLIENT_GEN="go run k8s.io/code-generator/cmd/client-gen"
CONTROLLER_GEN="go run sigs.k8s.io/controller-tools/cmd/controller-gen"
DEEPCOPY_GEN="go run k8s.io/code-generator/cmd/deepcopy-gen"
INFORMER_GEN="go run k8s.io/code-generator/cmd/informer-gen"
LISTER_GEN="go run k8s.io/code-generator/cmd/lister-gen"

echo "deleting generated code"
rm -rf ./internal/api/miniooperatorext/v1/generated.deepcopy.go
rm -rf ./internal/client

echo "running client-gen"
${CLIENT_GEN} \
    --clientset-name="clientset" \
    --input-base="${PKG}/internal/api" \
    --input="miniooperatorext/v1" \
    --output-dir="./internal/client" \
    --output-pkg="${PKG}/internal/client"

echo "running deepcopy-gen"
${DEEPCOPY_GEN} \
    "${PKG}/internal/api/miniooperatorext/v1"

echo "running lister-gen"
${LISTER_GEN} \
    --output-pkg="${PKG}/internal/client/lister" \
    --output-dir="./internal/client/lister" \
    "${PKG}/internal/api/miniooperatorext/v1"

echo "running informer-gen"
${INFORMER_GEN} \
    --listers-package="${PKG}/internal/client/lister" \
    --output-pkg="${PKG}/internal/client/informer" \
    --output-dir="./internal/client/informer" \
    --versioned-clientset-package="${PKG}/internal/client/clientset" \
    "${PKG}/internal/api/miniooperatorext/v1"
