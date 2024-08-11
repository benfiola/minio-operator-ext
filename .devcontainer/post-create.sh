#!/bin/sh
set -e
python -m venv /venv
. /venv/bin/activate
cd /workspaces/minio-operator-ext
pip install -e ".[dev]"

