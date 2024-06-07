import asyncio

from minio_operator_ext.operator import Operator


async def main():
    operator = Operator(kube_config="/root/.kube/config")
    await operator.run()


if __name__ == "__main__":
    asyncio.run(main())
