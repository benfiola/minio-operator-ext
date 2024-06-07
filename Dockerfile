FROM python:3.10.12 as operator

WORKDIR /app

ADD pyproject.toml pyproject.toml
ADD setup.py setup.py
ADD minio_operator_ext minio_operator_ext

RUN pip install -e .
ENTRYPOINT ["minio-operator-ext"]
CMD ["run"]
