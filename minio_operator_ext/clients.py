import dataclasses
import json
from typing import cast

import minio
import minio.crypto


@dataclasses.dataclass
class FakeEnum:
    """
    The minio client refers to API endpoints via an enum.

    Because the client in this package extends the minio client with support for additional enpdoints
    that currently aren't available upstream - this class presents an object that 'looks' like an enum
    (i.e., has a 'value' field).
    """

    value: str


class Minio(minio.Minio):
    """
    Subclass of the base minio client.
    """

    pass


class MinioAdmin(minio.MinioAdmin):
    """
    Subclass of the base minio admin client
    """

    def ldap_policy_set(
        self,
        policy_name: str | list[str],
        user: str | None = None,
        group: str | None = None,
    ) -> str:
        """
        Implements idp/ldap/policy/attach.  Roughly copies the implementation of 'policy_unset'.

        NOTE: the minio-py client is not up to date: https://github.com/minio/minio-py/issues/1337
        """
        if (user is not None) ^ (group is not None):
            policies = policy_name if isinstance(policy_name, list) else [policy_name]
            data: dict[str, str | list[str]] = {"policies": policies}
            if user:
                data["user"] = user
            if group:
                data["group"] = group
            response = self._url_open(
                "POST",
                FakeEnum(value="idp/ldap/policy/attach"),
                body=minio.crypto.encrypt(
                    json.dumps(data).encode(),
                    self._provider.retrieve().secret_key,
                ),
                preload_content=False,
            )
            plain_data = minio.crypto.decrypt(
                response,
                self._provider.retrieve().secret_key,
            )
            return plain_data.decode()
        raise ValueError("either user or group must be set")

    def ldap_policy_unset(
        self,
        policy_name: str | list[str],
        user: str | None = None,
        group: str | None = None,
    ) -> str:
        """
        Implements idp/ldap/policy/detach.  Copies the implementation of 'policy_unset'.

        NOTE: the minio-py client is not up to date: https://github.com/minio/minio-py/issues/1337
        """
        if (user is not None) ^ (group is not None):
            policies = policy_name if isinstance(policy_name, list) else [policy_name]
            data: dict[str, str | list[str]] = {"policies": policies}
            if user:
                data["user"] = user
            if group:
                data["group"] = group
            response = self._url_open(
                "POST",
                FakeEnum(value="idp/ldap/policy/detach"),
                body=minio.crypto.encrypt(
                    json.dumps(data).encode(),
                    self._provider.retrieve().secret_key,
                ),
                preload_content=False,
            )
            plain_data = minio.crypto.decrypt(
                response,
                self._provider.retrieve().secret_key,
            )
            return plain_data.decode()
        raise ValueError("either user or group must be set")
