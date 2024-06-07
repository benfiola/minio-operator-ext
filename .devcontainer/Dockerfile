FROM ubuntu:jammy

ARG ASDF_VERSION="0.14.0"
ARG PYTHON_VERSION="3.10.12"
ARG TARGETARCH

ENV DEBIAN_FRONTEND="noninteractive"

RUN apt -y update

# install python dependencies separately because it takes a long time
RUN apt -y install \ 
        build-essential \
        libssl-dev \
        zlib1g-dev \
        libbz2-dev \
        libreadline-dev \
        libsqlite3-dev \
        curl \
        libncursesw5-dev \
        xz-utils \
        tk-dev \
        libxml2-dev \
        libxmlsec1-dev \
        libffi-dev \
        liblzma-dev

# install asdf
RUN apt -y install \
        curl \
        git && \
    git clone https://github.com/asdf-vm/asdf.git /asdf --branch "v${ASDF_VERSION}" && \
    echo ". \"/asdf/asdf.sh\"" >> /root/.bashrc
ENV ASDF_DIR="/asdf"
ENV ASDF_DATA_DIR="${ASDF_DIR}"
ENV PATH="${ASDF_DIR}/shims:${ASDF_DIR}/bin:${PATH}"

# install asdf-python (note: dependencies installed earlier)
RUN asdf plugin add python && \
    asdf install python "${PYTHON_VERSION}" && \
    asdf global python "${PYTHON_VERSION}" && \
    asdf reshim

# install minikube
RUN apt -y install curl && \
    mkdir -p /opt/minikube && \
    cd /opt/minikube && \
    curl -o /opt/minikube/minikube -fsSL "https://storage.googleapis.com/minikube/releases/v1.33.1/minikube-linux-${TARGETARCH}" && \
    chmod +x /opt/minikube/minikube
ENV PATH="/opt/minikube:${PATH}"

# install kubectl
RUN apt -y install curl && \
    mkdir -p /opt/kubectl && \
    cd /opt/kubectl && \
    curl -o /opt/kubectl/kubectl -fsSL "https://dl.k8s.io/release/v1.29.4/bin/linux/${TARGETARCH}/kubectl" && \
    chmod +x /opt/kubectl/kubectl
ENV PATH="/opt/kubectl:${PATH}"

# install helm
RUN apt -y install curl && \
    mkdir -p /opt/helm && \
    cd /opt/helm && \
    curl -fsSL -o helm.tar.gz "https://get.helm.sh/helm-v3.14.3-linux-${TARGETARCH}.tar.gz" && \
    tar xvzf helm.tar.gz && \
    rm -rf helm.tar.gz
ENV PATH="/opt/helm/linux-${TARGETARCH}:${PATH}"

# install k9s
RUN apt -y install curl && \
    mkdir -p /opt/k9s && \
    cd /opt/k9s && \
    curl -fsSL -o k9s.tar.gz "https://github.com/derailed/k9s/releases/download/v0.32.4/k9s_Linux_${TARGETARCH}.tar.gz" && \
    tar xvzf k9s.tar.gz && \
    rm -rf k9s.tar.gz
ENV PATH="/opt/k9s:${PATH}"