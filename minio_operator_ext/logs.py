import logging

import minio_operator_ext


def configure_logging(log_level: int | None = None):
    """
    Configures logging for the application.
    """
    # configure the logger for the application
    log_level = log_level or logging.INFO
    logger = logging.getLogger(minio_operator_ext.__name__)
    handler = logging.StreamHandler()
    default_formatter = logging.Formatter(
        "%(asctime)s - %(levelname)s - %(name)s - %(message)s"
    )
    handler.setFormatter(default_formatter)
    logger.addHandler(handler)
    logger.setLevel(log_level)
