#!/bin/sh
set -ex

case $(uname -m) in
    x86_64)  arch="amd64" ;;
    arm64)   arch="arm64" ;;
    aarch64) arch="arm64" ;;
esac

case "${arch}" in
    amd64) alt_arch="x86_64" ;;
    *)     alt_arch="${arch}" ;;
esac

# install minikube
curl -o /usr/local/bin/minikube -fsSL "https://storage.googleapis.com/minikube/releases/v1.33.1/minikube-linux-${arch}"
chmod +x /usr/local/bin/minikube

# install kubectl
curl -o /usr/local/bin/kubectl -fsSL "https://dl.k8s.io/release/v1.29.4/bin/linux/${arch}/kubectl"
chmod +x /usr/local/bin/kubectl

# install k9s
curl -fsSL -o k9s.tar.gz "https://github.com/derailed/k9s/releases/download/v0.32.4/k9s_Linux_${arch}.tar.gz"
tar xvzf k9s.tar.gz -C /usr/local/bin
rm -rf k9s.tar.gz

# install helm
curl -fsSL -o helm.tar.gz "https://get.helm.sh/helm-v3.14.3-linux-${arch}.tar.gz"
tar xvzf helm.tar.gz --strip-components=1 -C /usr/local/bin
rm -rf helm.tar.gz

# install kubefwd
curl -fsSL -o kubefwd.tar.gz "https://github.com/txn2/kubefwd/releases/download/1.22.5/kubefwd_Linux_${alt_arch}.tar.gz"
tar xvzf kubefwd.tar.gz -C /usr/local/bin
rm -rf kubefwd.tar.gz

# install mc
curl -o /usr/local/bin/mc -fsSL "https://dl.min.io/client/mc/release/linux-${arch}/mc"
chmod +x /usr/local/bin/mc

# install kubebuilder
curl -o /usr/local/bin/kubebuilder -fsSL "https://github.com/kubernetes-sigs/kubebuilder/releases/download/v4.2.0/kubebuilder_linux_${arch}"
chmod +x /usr/local/bin/kubebuilder
