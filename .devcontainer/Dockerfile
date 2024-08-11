FROM ubuntu:jammy

ARG TARGETARCH

ENV DEBIAN_FRONTEND="noninteractive"

# install dependencies
RUN apt -y update && \
    apt -y install curl git

# install minikube
RUN mkdir -p /opt/minikube && \
    cd /opt/minikube && \
    curl -o /opt/minikube/minikube -fsSL "https://storage.googleapis.com/minikube/releases/v1.33.1/minikube-linux-${TARGETARCH}" && \
    chmod +x /opt/minikube/minikube
ENV PATH="/opt/minikube:${PATH}"

# install kubectl
RUN mkdir -p /opt/kubectl && \
    cd /opt/kubectl && \
    curl -o /opt/kubectl/kubectl -fsSL "https://dl.k8s.io/release/v1.29.4/bin/linux/${TARGETARCH}/kubectl" && \
    chmod +x /opt/kubectl/kubectl
ENV PATH="/opt/kubectl:${PATH}"

# install helm
RUN mkdir -p /opt/helm && \
    cd /opt/helm && \
    curl -fsSL -o helm.tar.gz "https://get.helm.sh/helm-v3.14.3-linux-${TARGETARCH}.tar.gz" && \
    tar xvzf helm.tar.gz && \
    rm -rf helm.tar.gz
ENV PATH="/opt/helm/linux-${TARGETARCH}:${PATH}"

# install k9s
RUN mkdir -p /opt/k9s && \
    cd /opt/k9s && \
    curl -fsSL -o k9s.tar.gz "https://github.com/derailed/k9s/releases/download/v0.32.4/k9s_Linux_${TARGETARCH}.tar.gz" && \
    tar xvzf k9s.tar.gz && \
    rm -rf k9s.tar.gz
ENV PATH="/opt/k9s:${PATH}"

# install kubefwd
RUN mkdir -p /opt/kubefwd && \
    cd /opt/kubefwd && \
    if [ "${TARGETARCH}" = "amd64" ]; then arch="x86_64"; else arch="${TARGETARCH}"; fi && \
    curl -fsSL -o kubefwd.tar.gz "https://github.com/txn2/kubefwd/releases/download/1.22.5/kubefwd_Linux_${arch}.tar.gz" && \
    tar xvzf kubefwd.tar.gz && \
    rm -rf kubefwd.tar.gz
ENV PATH="/opt/kubefwd:${PATH}"

# install mc
RUN mkdir -p /opt/mc && \
    cd /opt/mc && \
    curl -fsSL -o mc "https://dl.min.io/client/mc/release/linux-${TARGETARCH}/mc" && \
    chmod +x mc
ENV MC_DISABLE_PAGER=1
ENV MC_INSECURE=1
ENV PATH="/opt/mc:${PATH}"
