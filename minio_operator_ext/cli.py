import asyncio
import logging
import pathlib
from typing import Callable, TypeVar

import click
import uvloop

from minio_operator_ext.logs import configure_logging
from minio_operator_ext.operator import Operator

SomeCallable = TypeVar("SomeCallable", bound=Callable)


def log_level(value: str) -> int:
    """
    Click argument type that converts a log level string into
    a int log level compatible with the logging module
    """
    valid_values = ["debug", "info", "warning", "error"]
    if value.lower() not in valid_values:
        raise ValueError(value)
    return getattr(logging, value.upper())


def path(**kwargs) -> Callable[[str], pathlib.Path]:
    """
    Click argument type that mimics the `click.Path` argument type - but
    wraps the result in a `pathlib.Path` object.
    """
    click_decorator = click.Path(**kwargs)

    def inner(value: str) -> pathlib.Path:
        return pathlib.Path(click_decorator(value))

    return inner


def main():
    grp_main()


@click.group()
@click.option("--log-level", type=log_level, envvar="MINIO_OPERATOR_EXT_LOG_LEVEL")
def grp_main(log_level: int | None = None):
    uvloop.install()
    configure_logging(log_level)


@grp_main.command("run")
@click.option(
    "--kube-config",
    type=path(exists=True, dir_okay=False),
    envvar="MINIO_OPERATOR_EXT_KUBE_CONFIG",
    default=None,
)
def cmd_run(*, kube_config: pathlib.Path | None):
    async def inner():
        operator = Operator(kube_config=kube_config)
        await operator.run()

    asyncio.run(inner())


if __name__ == "__main__":
    main()
