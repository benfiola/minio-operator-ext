FROM python:3.10.12 as operator

WORKDIR /app

ADD operator/pyproject.toml pyproject.toml
ADD operator/setup.py setup.py
ADD operator/minio_operator_ext minio_operator_ext

RUN pip install -e .
ENTRYPOINT ["minio-operator-ext"]
CMD ["run"]
