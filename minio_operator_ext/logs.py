import logging
from typing import Literal

import minio_operator_ext

LogLevel = Literal["debug"] | Literal["info"] | Literal["warning"] | Literal["error"]

log_level_map: dict[LogLevel, int] = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


def configure_logging(log_level: LogLevel | None = None):
    """
    Configures logging for the application.
    """
    # configure the logger for the application
    log_level = log_level or "info"

    logger = logging.getLogger(minio_operator_ext.__name__)
    handler = logging.StreamHandler()
    default_formatter = logging.Formatter(
        "%(asctime)s - %(levelname)s - %(name)s - %(message)s"
    )
    handler.setFormatter(default_formatter)
    logger.addHandler(handler)
    logger.setLevel(log_level_map[log_level])
