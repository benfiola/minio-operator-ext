import asyncio
import pathlib

from minio_operator_ext.logs import configure_logging
from minio_operator_ext.operator import Operator


async def main():
    operator = Operator(kube_config=pathlib.Path("/root/.kube/config"))
    await operator.run()


if __name__ == "__main__":
    asyncio.run(main())
