import asyncio
import pathlib
from typing import Callable, TypeVar

import click
import dotenv
import pydantic
import uvloop
from minio_operator_ext.logs import LogLevel, configure_logging
from minio_operator_ext.operator import Operator

SomeCallable = TypeVar("SomeCallable", bound=Callable)


def log_level(value: str) -> LogLevel:
    """
    Click argument type that produces a 'LogLevel' from an incoming value string.
    """
    return pydantic.TypeAdapter(LogLevel).validate_python(value)


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


def load_env_file(env_file: pathlib.Path):
    """
    Callback that loads an env file.  Ths is implemented as a callback to ensure
    that env files are loaded as early as possible so that other arguments can be correctly
    processed (as they can be set by environment variables).

    NOTE: https://click.palletsprojects.com/en/8.1.x/options/#callbacks-and-eager-options
    NOTE: Used as option to `grp_main`.
    """
    dotenv.load_dotenv(env_file)


@click.group()
@click.option(
    "--env-file",
    type=path(exists=True, dir_okay=False),
    callback=lambda *args: load_env_file(args[2]),
    is_eager=True,
    expose_value=False,
)
@click.option("--log-level", type=log_level, envvar="MINIO_OPERATOR_EXT_LOG_LEVEL")
def grp_main(log_level: LogLevel | None = None):
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
