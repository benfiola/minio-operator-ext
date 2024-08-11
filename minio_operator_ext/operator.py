import asyncio
import base64
import contextlib
import io
import logging
import pathlib
import tempfile
from typing import AsyncGenerator, Callable, TypeVar, cast

import dotenv
import lightkube.resources.core_v1
import minio
import minio.credentials.providers
import minio_operator_ext.clients as clients
import minio_operator_ext.resources as resources
import operator_core
import pydantic

logger = logging.getLogger(__name__)


UnwrapV = TypeVar("UnwrapV")


class Tenant(pydantic.BaseModel):
    """
    Represents a simplified data container pointing to a minio.min.io/Tenant resource.
    """

    access_key: str
    endpoint: str
    secret_key: str
    secure: bool
    ca_bundle: str | None = None


class Bucket(pydantic.BaseModel):
    """
    Represents a user with all references resolved
    """

    name: str
    tenant: Tenant


class User(pydantic.BaseModel):
    """
    Represents a user with all references resolved
    """

    access_key: str
    secret_key: str
    tenant: Tenant


class Group(pydantic.BaseModel):
    """
    Represents a group with all references resolved
    """

    name: str
    tenant: Tenant


class GroupBinding(pydantic.BaseModel):
    """
    Represents a group binding with all references resolved
    """

    group: str
    tenant: Tenant
    user: str


class Policy(pydantic.BaseModel):
    """
    Represents a policy with all references resolved
    """

    statement: list[resources.PolicyStatement]
    name: str
    tenant: Tenant
    version: str


class PolicyBinding(pydantic.BaseModel):
    """
    Represents a policy binding with all references resolved
    """

    group: resources.MinioPolicyIdentity | None
    user: resources.MinioPolicyIdentity | None
    policy: str
    tenant: Tenant


SyncRV = TypeVar("SyncRV")


class Operator(operator_core.Operator):
    """
    Implements a kubernetes operator capable of syncing minio tenants and
    a handful of custom resource definitions with a remote minio server.
    """

    def __init__(self, **kwargs):
        kwargs["logger"] = logger
        super().__init__(**kwargs)

        self.watch_resource(
            resources.MinioBucket,
            on_create=self.create_minio_bucket,
            on_delete=self.delete_minio_bucket,
        )
        self.watch_resource(
            resources.MinioGroup,
            on_create=self.create_minio_group,
            on_delete=self.delete_minio_group,
        )
        self.watch_resource(
            resources.MinioGroupBinding,
            on_create=self.create_minio_group_binding,
            on_delete=self.delete_minio_group_binding,
        )
        self.watch_resource(
            resources.MinioPolicy,
            on_create=self.create_minio_policy,
            on_update=self.update_minio_policy,
            on_delete=self.delete_minio_policy,
        )
        self.watch_resource(
            resources.MinioPolicyBinding,
            on_create=self.create_minio_policy_binding,
            on_delete=self.delete_minio_policy_binding,
        )
        self.watch_resource(
            resources.MinioUser,
            on_create=self.create_minio_user,
            on_update=self.update_minio_user,
            on_delete=self.delete_minio_user,
        )

    async def run_sync(self, callable: Callable[[], SyncRV]) -> SyncRV:
        return await asyncio.get_running_loop().run_in_executor(None, callable)

    @contextlib.asynccontextmanager
    async def temporary_file(self, **kwargs) -> AsyncGenerator[pathlib.Path, None]:
        """
        Defines an async context manager that mimics that of `tempfile.NamedTemporaryFile`.

        Rather than return a file handle, returns a `pathlib.Path` object.
        """
        with tempfile.NamedTemporaryFile(**kwargs) as handle:
            yield pathlib.Path(handle.name)

    @contextlib.asynccontextmanager
    async def create_minio_client(
        self, tenant: Tenant
    ) -> AsyncGenerator[clients.Minio, None]:
        """
        Creates a minio client from the given tenant.
        """
        async with self.temporary_file(suffix=".crt") as ca_cert:
            client = clients.Minio(
                access_key=tenant.access_key,
                endpoint=tenant.endpoint,
                secret_key=tenant.secret_key,
                secure=tenant.secure,
            )
            if tenant.secure and tenant.ca_bundle:
                ca_cert.write_text(tenant.ca_bundle)
                client._http.connection_pool_kw["ca_certs"] = f"{ca_cert}"
            yield client

    @contextlib.asynccontextmanager
    async def create_minio_admin_client(
        self,
        tenant: Tenant,
    ) -> AsyncGenerator[clients.MinioAdmin, None]:
        """
        Creates a minio admin client from the given tenant
        """
        async with self.temporary_file(suffix=".crt") as ca_cert:
            client = clients.MinioAdmin(
                credentials=minio.credentials.providers.StaticProvider(
                    access_key=tenant.access_key, secret_key=tenant.secret_key
                ),
                endpoint=tenant.endpoint,
                secure=tenant.secure,
            )
            if tenant.secure and tenant.ca_bundle:
                ca_cert.write_text(tenant.ca_bundle)
                client._http.connection_pool_kw["ca_certs"] = f"{ca_cert}"
            yield client

    async def resolve_tenant(self, resource: resources.Tenant) -> Tenant:
        """
        Builds a `Tenant` object from information fetched from
        resources within the kubernetes cluster.
        """
        namespace = resource.metadata.namespace or "default"

        # determine whether tenant uses http or https
        secure = resource.spec.requestAutoCert

        # if the tenant uses https, fetch ca bundle used to verify self-signed cert
        # NOTE: this will need to eventually support other certificate sources (e.g., cert-manager)
        ca_bundle = None
        if secure:
            ca_bundle_config_map = (
                await self.kube_client.get(
                    lightkube.resources.core_v1.ConfigMap,
                    name="kube-root-ca.crt",
                    namespace=namespace,
                )
            ).to_dict()
            ca_bundle = ca_bundle_config_map["data"]["ca.crt"]

        # extract credentials from tenant secret
        configuration_name = resource.spec.configuration.name
        configuration = (
            await self.kube_client.get(
                lightkube.resources.core_v1.Secret,
                name=configuration_name,
                namespace=namespace,
            )
        ).to_dict()
        env_file = configuration["data"]["config.env"]
        env_file = base64.b64decode(env_file).decode("utf-8")
        env_file = env_file.replace("export ", "")
        env_data = dotenv.dotenv_values(stream=io.StringIO(env_file))
        access_key = env_data["MINIO_ROOT_USER"]
        secret_key = env_data["MINIO_ROOT_PASSWORD"]
        if not access_key or not secret_key:
            raise operator_core.OperatorError(
                f"credentials not found in secret: {namespace}/{configuration_name}"
            )

        # determine endpoint
        # (NOTE: assumes service name 'minio' from helm templates)
        service_name = "minio"
        service = (
            await self.kube_client.get(
                lightkube.resources.core_v1.Service,
                name=service_name,
                namespace=namespace,
            )
        ).to_dict()
        if service is None:
            raise operator_core.OperatorError(
                f"tenant service not found: {namespace}/{service_name}",
                recoverable=True,
            )
        service_port: int | None = None
        service_port_name = "http-minio"
        if secure:
            service_port_name = "https-minio"
        for port in service["spec"]["ports"]:
            if port["name"] != service_port_name:
                continue
            service_port = port["port"]
            break
        if not service_port:
            raise operator_core.OperatorError(
                f"port not found in service: {namespace}/{service_name}"
            )

        svc_namespace = service["metadata"]["namespace"]
        svc_name = service["metadata"]["name"]
        endpoint = f"{svc_name}.{svc_namespace}.svc:{service_port}"

        return Tenant(
            access_key=access_key,
            ca_bundle=ca_bundle,
            endpoint=endpoint,
            secret_key=secret_key,
            secure=secure,
        )

    async def resolve_minio_bucket(self, resource: resources.MinioBucket) -> Bucket:
        """
        Resolves a bucket spec to a bucket - translating references to actual values.
        """
        namespace = resource.metadata.namespace or "default"
        tenant_resource = await self.kube_client.get(
            resources.Tenant,
            name=resource.spec.tenantRef.name,
            namespace=resource.spec.tenantRef.namespace or namespace,
        )
        tenant = await self.resolve_tenant(tenant_resource)
        return Bucket(name=resource.spec.name, tenant=tenant)

    async def create_minio_bucket(self, resource: resources.MinioBucket, **kwargs):
        """
        Creates a minio bucket
        """
        bucket = await self.resolve_minio_bucket(resource)

        async with self.create_minio_client(bucket.tenant) as minio_client:

            def inner():
                try:
                    minio_client.make_bucket(bucket.name)
                except minio.error.S3Error as e:
                    if e.code == "BucketAlreadyOwnedByYou":
                        raise operator_core.OperatorError(
                            f"bucket already exists: {bucket.name}"
                        )
                    raise e

            await self.run_sync(inner)

    async def delete_minio_bucket(self, resource: resources.MinioBucket, **kwargs):
        """
        Deletes a minio bucket
        """
        bucket = await self.resolve_minio_bucket(resource)

        async with self.create_minio_client(bucket.tenant) as minio_client:

            def inner():
                try:
                    minio_client.remove_bucket(bucket.name)
                except minio.error.S3Error as e:
                    if e.code == "NoSuchBucket":
                        return
                    raise e

            await self.run_sync(inner)

    async def resolve_minio_user(self, resource: resources.MinioUser) -> User:
        """
        Resolves a user spec to a user - translating references to actual values.
        """
        namespace = resource.metadata.namespace or "default"
        secret = (
            await self.kube_client.get(
                lightkube.resources.core_v1.Secret,
                name=resource.spec.secretKeyRef.name,
                namespace=namespace,
            )
        ).to_dict()
        secret_key = secret["data"][resource.spec.secretKeyRef.key]
        secret_key = base64.b64decode(secret_key).decode("utf-8")

        tenant_resource = await self.kube_client.get(
            resources.Tenant,
            name=resource.spec.tenantRef.name,
            namespace=resource.spec.tenantRef.namespace or namespace,
        )
        tenant = await self.resolve_tenant(tenant_resource)

        return User(
            access_key=resource.spec.accessKey,
            secret_key=secret_key,
            tenant=tenant,
        )

    async def create_minio_user(self, resource: resources.MinioUser, **kwargs):
        """
        Creates a minio user
        """
        user = await self.resolve_minio_user(resource)

        async with self.create_minio_admin_client(user.tenant) as minio_admin_client:

            def inner():
                # NOTE: the `user_add` endpoint will succeed even if a user with the access key already exists
                try:
                    minio_admin_client.user_info(user.access_key)
                    raise operator_core.OperatorError(
                        f"user already exists: {user.access_key}"
                    )
                except minio.error.MinioAdminException as e:
                    if e._code != "404":
                        raise e

                minio_admin_client.user_add(user.access_key, user.secret_key)

            await self.run_sync(inner)

    async def update_minio_user(self, resource: resources.MinioUser, **kwargs):
        """
        Creates a minio user
        """
        user = await self.resolve_minio_user(resource)

        async with self.create_minio_admin_client(user.tenant) as minio_admin_client:

            def inner():
                minio_admin_client.user_add(user.access_key, user.secret_key)

            await self.run_sync(inner)

    async def delete_minio_user(self, resource: resources.MinioUser, **kwargs):
        """
        Deletes a minio user
        """
        user = await self.resolve_minio_user(resource)

        async with self.create_minio_admin_client(user.tenant) as minio_admin_client:

            def inner():
                try:
                    minio_admin_client.user_remove(user.access_key)
                except minio.error.MinioAdminException as e:
                    if e._code == "404":
                        return
                    raise e

            await self.run_sync(inner)

    async def resolve_minio_group(self, resource: resources.MinioGroup) -> Group:
        """
        Resolves a group spec to a group - translating references to actual values.
        """
        namespace = resource.metadata.namespace or "default"

        tenant_resource = await self.kube_client.get(
            resources.Tenant,
            name=resource.spec.tenantRef.name,
            namespace=resource.spec.tenantRef.namespace or namespace,
        )
        tenant = await self.resolve_tenant(tenant_resource)

        return Group(
            name=resource.spec.name,
            tenant=tenant,
        )

    async def create_minio_group(self, resource: resources.MinioGroup, **kwargs):
        """
        Creates a minio group
        """
        group = await self.resolve_minio_group(resource)

        async with self.create_minio_admin_client(group.tenant) as minio_admin_client:

            def inner():
                # NOTE: the `group_add` endpoint will succeed even when a group already exists
                try:
                    minio_admin_client.group_info(group.name)
                    raise operator_core.OperatorError(
                        f"group already exists: {group.name}"
                    )
                except minio.error.MinioAdminException as e:
                    if e._code != "404":
                        raise e

                # NOTE: this api is incorrectly typed (the member list is typed as 'str' - should be 'list[str]')
                minio_admin_client.group_add(group.name, cast(str, []))

            await self.run_sync(inner)

    async def delete_minio_group(self, resource: resources.MinioGroup, **kwargs):
        """
        Deletes a minio group
        """
        group = await self.resolve_minio_group(resource)

        async with self.create_minio_admin_client(group.tenant) as minio_admin_client:

            def inner():
                try:
                    minio_admin_client.group_remove(group.name)
                except minio.error.MinioAdminException as e:
                    if e._code == "404":
                        return
                    raise e

            await self.run_sync(inner)

    async def resolve_minio_group_binding(
        self, resource: resources.MinioGroupBinding
    ) -> GroupBinding:
        """
        Resolves a group binding spec to a group binding - translating references to actual values.
        """
        namespace = resource.metadata.namespace or "default"

        tenant_resource = await self.kube_client.get(
            resources.Tenant,
            name=resource.spec.tenantRef.name,
            namespace=resource.spec.tenantRef.namespace or namespace,
        )
        tenant = await self.resolve_tenant(tenant_resource)

        return GroupBinding(
            group=resource.spec.group,
            tenant=tenant,
            user=resource.spec.user,
        )

    async def create_minio_group_binding(
        self, resource: resources.MinioGroupBinding, **kwargs
    ):
        """
        Adds a user to a group
        """
        group_binding = await self.resolve_minio_group_binding(resource)

        async with self.create_minio_admin_client(
            group_binding.tenant
        ) as minio_admin_client:

            def inner():
                # NOTE: ensure group already exists before calling 'group_add' to modify members
                try:
                    minio_admin_client.group_info(group_binding.group)
                except minio.error.MinioAdminException as e:
                    if e._code != "404":
                        raise e
                    raise operator_core.OperatorError(
                        f"group does not exist: {group_binding.group}", recoverable=True
                    )

                # NOTE: this api is incorrectly typed (the member list is typed as 'str' - should be 'list[str]')
                minio_admin_client.group_add(
                    group_binding.group, cast(str, [group_binding.user])
                )

            await self.run_sync(inner)

    async def delete_minio_group_binding(
        self, resource: resources.MinioGroupBinding, **kwargs
    ):
        """
        Removes a user from a group
        """
        group_binding = await self.resolve_minio_group_binding(resource)

        async with self.create_minio_admin_client(
            group_binding.tenant
        ) as minio_admin_client:

            def inner():
                try:
                    # NOTE: this api is incorrectly typed (the member list is typed as 'str' - should be 'list[str]')
                    minio_admin_client.group_remove(
                        group_binding.group, cast(str, [group_binding.user])
                    )
                except minio.error.MinioAdminException as e:
                    if e._code == "404":
                        return
                    raise e

            await self.run_sync(inner)

    async def resolve_minio_policy(self, resource: resources.MinioPolicy) -> Policy:
        """
        Resolves a policy spec to a policy - translating references to actual values.
        """
        namespace = resource.metadata.namespace or "default"

        tenant_resource = await self.kube_client.get(
            resources.Tenant,
            name=resource.spec.tenantRef.name,
            namespace=resource.spec.tenantRef.namespace or namespace,
        )
        tenant = await self.resolve_tenant(tenant_resource)

        return Policy(
            name=resource.spec.name,
            statement=resource.spec.statement,
            tenant=tenant,
            version=resource.spec.version,
        )

    @contextlib.asynccontextmanager
    async def minio_policy_file(
        self,
        policy: Policy,
    ) -> AsyncGenerator[pathlib.Path, None]:
        """
        Provides a context that writes a given policy to a policy file suitable for use with the minio admin apis.
        """
        async with self.temporary_file(suffix=".json") as policy_file:
            data = policy.model_dump_json(include={"statement", "version"})
            policy_file.write_text(data)
            yield policy_file

    async def create_minio_policy(self, resource: resources.MinioPolicy, **kwargs):
        """
        Creates a minio policy
        """
        policy = await self.resolve_minio_policy(resource)

        async with self.create_minio_admin_client(policy.tenant) as minio_admin_client:
            async with self.minio_policy_file(policy) as policy_file:

                def inner():
                    # NOTE: the `policy_add` endpoint will succeed even if a policy with the given name already exists
                    try:
                        minio_admin_client.policy_info(policy.name)
                        raise operator_core.OperatorError(
                            f"policy already exists: {policy.name}"
                        )
                    except minio.error.MinioAdminException as e:
                        if e._code != "404":
                            raise e

                    minio_admin_client.policy_add(policy.name, f"{policy_file}")

                return await self.run_sync(inner)

    async def update_minio_policy(self, resource: resources.MinioPolicy, **kwargs):
        """
        Updates a minio policy
        """
        policy = await self.resolve_minio_policy(resource)

        async with self.create_minio_admin_client(policy.tenant) as minio_admin_client:
            async with self.minio_policy_file(policy) as policy_file:

                def inner():
                    minio_admin_client.policy_add(policy.name, f"{policy_file}")

                await self.run_sync(inner)

    async def delete_minio_policy(self, resource: resources.MinioPolicy, **kwargs):
        """
        Deletes a minio policy
        """
        policy = await self.resolve_minio_policy(resource)
        async with self.create_minio_admin_client(policy.tenant) as minio_admin_client:

            def inner():
                minio_admin_client.policy_remove(policy.name)

            await self.run_sync(inner)

    async def resolve_minio_policy_binding(
        self, resource: resources.MinioPolicyBinding
    ) -> PolicyBinding:
        """
        Resolves a policy binding spec to a policy binding - translating references to actual values.
        """
        namespace = resource.metadata.namespace or "default"

        tenant_resource = await self.kube_client.get(
            resources.Tenant,
            name=resource.spec.tenantRef.name,
            namespace=resource.spec.tenantRef.namespace or namespace,
        )
        tenant = await self.resolve_tenant(tenant_resource)

        return PolicyBinding(
            group=resource.spec.group,
            policy=resource.spec.policy,
            tenant=tenant,
            user=resource.spec.user,
        )

    async def create_minio_policy_binding(
        self, resource: resources.MinioPolicyBinding, **kwargs
    ):
        """
        Attaches a user/group to a policy
        """
        policy_binding = await self.resolve_minio_policy_binding(resource)

        async with self.create_minio_admin_client(
            policy_binding.tenant
        ) as minio_admin_client:

            def inner():
                group = policy_binding.group
                user = policy_binding.user

                builtin_group = group and group.builtin
                builtin_user = user and user.builtin
                ldap_group = group and group.ldap
                ldap_user = user and user.ldap

                if builtin_group or builtin_user:
                    minio_admin_client.policy_set(
                        policy_binding.policy,
                        group=builtin_group,
                        user=builtin_user,
                    )
                elif ldap_group or ldap_user:
                    minio_admin_client.ldap_policy_set(
                        policy_binding.policy, group=ldap_group, user=ldap_user
                    )

            await self.run_sync(inner)

    async def delete_minio_policy_binding(
        self, resource: resources.MinioPolicyBinding, **kwargs
    ):
        """
        Detaches a user/group from a policy
        """
        policy_binding = await self.resolve_minio_policy_binding(resource)

        async with self.create_minio_admin_client(
            policy_binding.tenant
        ) as minio_admin_client:

            def inner():
                try:
                    group = policy_binding.group
                    user = policy_binding.user

                    builtin_group = group and group.builtin
                    builtin_user = user and user.builtin
                    ldap_group = group and group.ldap
                    ldap_user = user and user.ldap

                    if builtin_group or builtin_user:
                        minio_admin_client.policy_unset(
                            policy_binding.policy,
                            group=builtin_group,
                            user=builtin_user,
                        )
                    elif ldap_group or ldap_user:
                        minio_admin_client.ldap_policy_unset(
                            policy_binding.policy,
                            group=ldap_group,
                            user=ldap_user,
                        )
                except minio.error.MinioAdminException as e:
                    if e._code == "400":
                        if "policy change is already in effect" in e._body:
                            return
                    raise e

            await self.run_sync(inner)
