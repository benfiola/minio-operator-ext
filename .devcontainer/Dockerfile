FROM ubuntu:jammy

ARG ASDF_VERSION="0.14.0"
ARG PYTHON_VERSION="3.10.12"

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