import contextlib
import io
import logging
import pathlib
import tempfile
from typing import Any, AsyncGenerator, cast

import dotenv
import kopf
import minio
import minio.credentials.providers
import operator_core
import pydantic

logger = logging.getLogger(__name__)


class KnownClusterResourceRefSpec(operator_core.BaseModel):
    """
    A 'known' cluster resource ref spec is a resource ref whose api version and plural are contextually known.

    (e.g., one can infer that 'namespaceRef' has plural 'namespaces' and apiVersion 'v1')
    """

    name: str


class KnownResourceRefSpec(operator_core.BaseModel):
    """
    A 'known' resource ref spec is a resource ref whose api version and plural are contextually known.

    Additionally, the namespace can be omitted - as one can infer the namespace from the parent resource.

    (e.g., one can infer that 'secretRef' has plural 'secrets' and apiVersion 'v1')
    """

    name: str
    namespace: str | None = None


class KnownResourceKeyRefSpec(operator_core.KnownResourceRefSpec):
    """
    A 'known' resource key ref spec carries all the assumptions of a 'KnownResourceRefSpec' but is intended to
    additionally require a 'key' field to reference an individual member of a parent resource (e.g., config maps + secrets)
    """

    key: str


class Tenant(operator_core.BaseModel):
    """
    Represents a simplified data container pointing to a minio.min.io/Tenant resource.
    """

    access_key: str
    ca_bundle: str | None = None
    endpoint: str
    resource: operator_core.ResourceRef
    secret_key: str
    secure: bool


class BucketSpec(operator_core.BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioBucket resource.
    """

    name: str
    tenant_ref: KnownResourceRefSpec = pydantic.Field(alias="tenantRef")


class Bucket(operator_core.BaseModel):
    name: str
    resource: operator_core.ResourceRef
    tenant: Tenant


class UserSpec(operator_core.BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioUser resource.
    """

    access_key: str = pydantic.Field(alias="accessKey")
    secret_key_ref: KnownResourceKeyRefSpec = pydantic.Field(
        alias="secretKeyRef"
    )
    tenant_ref: KnownResourceRefSpec = pydantic.Field(alias="tenantRef")


class User(operator_core.BaseModel):
    """
    Represents a user with all references resolved
    """

    access_key: str
    secret_key: str
    resource: operator_core.ResourceRef
    tenant: Tenant


class GroupSpec(operator_core.BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioGroup resource.
    """

    name: str
    tenant_ref: KnownResourceRefSpec = pydantic.Field(alias="tenantRef")


class Group(operator_core.BaseModel):
    """
    Represents a group with all references resolved
    """

    name: str
    resource: operator_core.ResourceRef
    tenant: Tenant


class GroupBindingSpec(operator_core.BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioGroupBinding resource.
    """

    group: str
    tenant_ref: KnownResourceRefSpec = pydantic.Field(alias="tenantRef")
    user: str


class GroupBinding(operator_core.BaseModel):
    """
    Represents a group binding with all references resolved
    """

    group: str
    resource: operator_core.ResourceRef
    tenant: Tenant
    user: str


class PolicyStatement(operator_core.BaseModel):
    """
    Represents the spec.statement subfield of a bfiola.dev/MinioPolicy resource.
    """

    action: list[str]
    effect: str
    resource: list[str]


class PolicySpec(operator_core.BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioPolicy resource.
    """

    statement: list[PolicyStatement]
    name: str
    tenant_ref: KnownResourceRefSpec = pydantic.Field(alias="tenantRef")
    version: str


class Policy(operator_core.BaseModel):
    """
    Represents a policy with all references resolved
    """

    statement: list[PolicyStatement]
    name: str
    resource: operator_core.ResourceRef
    tenant: Tenant
    version: str


class PolicyBindingSpec(operator_core.BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioPolicyBinding resource.
    """

    group: str | None = None
    user: str | None = None
    policy: str
    tenant_ref: KnownResourceRefSpec = pydantic.Field(alias="tenantRef")

    @pydantic.model_validator(mode="before")
    @classmethod
    def ensure_group_or_user_set(cls, data: Any) -> Any:
        fields = [data.get("group"), data.get("user")]
        if all(fields) or not any(fields):
            raise ValueError(f"only one of [group, user] must be provided")
        return data


class PolicyBinding(operator_core.BaseModel):
    """
    Represents a policy binding with all references resolved
    """

    group: str | None
    user: str | None
    policy: str
    resource: operator_core.ResourceRef
    tenant: Tenant


class Operator(operator_core.Operator):
    """
    Implements a kubernetes operator capable of syncing minio tenants and
    a handful of custom resource definitions with a remote minio server.
    """

    def __init__(self, **kwargs):
        kwargs["logger"] = logger
        super().__init__(**kwargs)

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
    ) -> AsyncGenerator[minio.Minio, None]:
        """
        Creates a minio client from the given tenant.
        """
        async with self.temporary_file(suffix=".crt") as ca_cert:
            client = minio.Minio(
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
    ) -> AsyncGenerator[minio.MinioAdmin, None]:
        """
        Creates a minio admin client from the given tenant
        """
        async with self.temporary_file(suffix=".crt") as ca_cert:
            client = minio.MinioAdmin(
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

    async def get_tenant(self, ref: operator_core.ResourceRef) -> Tenant:
        """
        Builds a `Tenant` object from information fetched from
        resources within the kubernetes cluster.
        """
        # get tenant resource
        tenant = await self.get_resource(ref)
        if tenant is None:
            raise operator_core.OperatorError(
                f"tenant not found: {ref.fqn}", recoverable=True
            )

        # determine whether tenant uses http or https
        secure = tenant["spec"]["requestAutoCert"]

        # if the tenant uses https, fetch ca bundle used to verify self-signed cert
        # NOTE: this will need to eventually support other certificate sources (e.g., cert-manager)
        ca_bundle = None
        if secure:
            ca_bundle_ref = operator_core.ResourceRef(
                api_version="v1",
                plural="configmaps",
                name="kube-root-ca.crt",
                namespace=ref.namespace,
            )
            ca_bundle_config_map = await self.get_resource(ca_bundle_ref)
            if ca_bundle_config_map is None:
                raise operator_core.OperatorError(
                    f"ca bundle config map not found: {ca_bundle_ref.fqn}",
                    recoverable=True,
                )
            ca_bundle = self.get_resource_key(ca_bundle_config_map, "ca.crt")

        # extract credentials from tenant secret
        configuration_ref = operator_core.ResourceRef(
            api_version="v1",
            plural="secrets",
            name=tenant["spec"]["configuration"]["name"],
            namespace=ref.namespace,
        )
        configuration_secret = await self.get_resource(configuration_ref)
        if configuration_secret is None:
            raise operator_core.OperatorError(
                f"tenant configuration secret not found: {configuration_ref.fqn}",
                recoverable=True,
            )
        env_file = self.get_resource_key(configuration_secret, "config.env")
        env_file = env_file.replace("export ", "")
        env_data = dotenv.dotenv_values(stream=io.StringIO(env_file))
        access_key = env_data["MINIO_ROOT_USER"]
        secret_key = env_data["MINIO_ROOT_PASSWORD"]
        if not access_key or not secret_key:
            raise operator_core.OperatorError(
                f"credentials not found in secret: {operator_core.resource_fqn(configuration_secret)}"
            )

        # determine endpoint
        # (NOTE: assumes service name 'minio' from helm templates)
        service_ref = operator_core.ResourceRef(
            api_version="v1", plural="services", name="minio", namespace=ref.namespace
        )
        service = await self.get_resource(service_ref)
        if service is None:
            raise operator_core.OperatorError(
                f"tenant service not found: {service_ref}", recoverable=True
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
                f"port not found in service: {operator_core.resource_fqn(service)}"
            )
        endpoint = f"{service_ref.name}.{service_ref.namespace}.svc:{service_port}"

        return Tenant(
            access_key=access_key,
            ca_bundle=ca_bundle,
            endpoint=endpoint,
            resource=ref,
            secret_key=secret_key,
            secure=secure,
        )

    async def resolve_minio_bucket_spec(
        self, bucket_spec: BucketSpec, body: kopf.Body
    ) -> Bucket:
        """
        Resolves a bucket spec to a bucket - translating references to actual values.
        """
        namespace = operator_core.resource_namespace(body)

        resource = operator_core.ResourceRef(
            api_version="bfiola.dev/v1",
            plural="miniobuckets",
            name=body["metadata"]["name"],
            namespace=namespace,
        )

        tenant_ref = operator_core.ResourceRef(
            api_version="minio.min.io/v2",
            plural="tenants",
            name=bucket_spec.tenant_ref.name,
            namespace=bucket_spec.tenant_ref.namespace or namespace,
        )
        tenant = await self.get_tenant(tenant_ref)

        return Bucket(name=bucket_spec.name, resource=resource, tenant=tenant)

    async def create_minio_bucket(self, bucket: Bucket):
        """
        Creates a new minio bucket given the provided bucket resource
        """
        async with self.create_minio_client(bucket.tenant) as minio_client:

            def inner():
                try:
                    minio_client.make_bucket(bucket.name)
                except minio.error.S3Error as e:
                    if e.code == "BucketAlreadyOwnedByYou":
                        raise operator_core.OperatorError(
                            f"bucket already exists: {bucket.tenant.resource.fqn}/{bucket.name}"
                        )
                    raise e

            await operator_core.run_sync(inner)

    async def update_minio_bucket(self, bucket: Bucket):
        """
        Updates an existing minio bucket given the provided bucket resource
        """
        async with self.create_minio_client(bucket.tenant) as minio_client:

            def inner():
                pass

            await operator_core.run_sync(inner)

    async def delete_minio_bucket(self, bucket: Bucket):
        """
        Deletes an existing minio bucket given the provided bucket resource
        """
        async with self.create_minio_client(bucket.tenant) as minio_client:

            def inner():
                try:
                    minio_client.remove_bucket(bucket.name)
                except minio.error.S3Error as e:
                    if e.code == "NoSuchBucket":
                        return
                    raise e

            await operator_core.run_sync(inner)

    @operator_core.hook("create", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is created
        """
        spec = BucketSpec.model_validate(body["spec"])
        bucket = await self.resolve_minio_bucket_spec(spec, body)
        await self.create_minio_bucket(bucket)
        patch.status["currentSpec"] = spec.model_dump()

    @operator_core.hook("update", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = BucketSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            bucket = await self.resolve_minio_bucket_spec(new_spec, body)
            await self.create_minio_bucket(bucket)
            patch.status["currentSpec"] = new_spec.model_dump()
            return

        current_spec = BucketSpec.model_validate(current_spec)
        immutable = {("tenantRef",), ("name",)}
        diff = operator_core.get_diff(current_spec, new_spec)
        diff = operator_core.filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = operator_core.apply_diff_item(current_spec, item)
            bucket = await self.resolve_minio_bucket_spec(current_spec, body)
            await self.update_minio_bucket(bucket)
            patch.status["currentSpec"] = current_spec.model_dump()

    @operator_core.hook("delete", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = BucketSpec.model_validate(current_spec)
            bucket = await self.resolve_minio_bucket_spec(current_spec, body)
        except Exception as e:
            return
        await self.delete_minio_bucket(bucket)

    async def resolve_minio_user_spec(
        self, user_spec: UserSpec, body: kopf.Body
    ) -> User:
        """
        Resolves a user spec to a user - translating references to actual values.
        """
        namespace = operator_core.resource_namespace(body)

        resource = operator_core.ResourceRef(
            api_version="bfiola.dev/v1",
            plural="miniousers",
            name=body["metadata"]["name"],
            namespace=namespace,
        )

        secret_ref = operator_core.ResourceRef(
            api_version="v1",
            plural="secrets",
            name=user_spec.secret_key_ref.name,
            namespace=user_spec.secret_key_ref.namespace or namespace,
        )
        secret = await self.get_resource(secret_ref)
        if secret is None:
            raise operator_core.OperatorError(
                f"user secret not found: {secret_ref.fqn}", recoverable=True
            )
        secret_key = self.get_resource_key(secret, user_spec.secret_key_ref.key)

        tenant_ref = operator_core.ResourceRef(
            api_version="minio.min.io/v2",
            plural="tenants",
            name=user_spec.tenant_ref.name,
            namespace=user_spec.tenant_ref.namespace or namespace,
        )
        tenant = await self.get_tenant(tenant_ref)

        return User(
            access_key=user_spec.access_key,
            secret_key=secret_key,
            resource=resource,
            tenant=tenant,
        )

    async def create_minio_user(self, user: User):
        """
        Creates a new user given the provided user resource
        """
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

            await operator_core.run_sync(inner)

    async def update_minio_user(self, user: User):
        """
        Updates an existing user given the provided user resource
        """
        async with self.create_minio_admin_client(user.tenant) as minio_admin_client:

            def inner():
                minio_admin_client.user_add(user.access_key, user.secret_key)

            await operator_core.run_sync(inner)

    async def delete_minio_user(self, user: User):
        """
        Deletes an existing user given the provided user resource
        """
        async with self.create_minio_admin_client(user.tenant) as minio_admin_client:

            def inner():
                try:
                    minio_admin_client.user_remove(user.access_key)
                except minio.error.MinioAdminException as e:
                    if e._code == "404":
                        return
                    raise e

            await operator_core.run_sync(inner)

    @operator_core.hook("create", "bfiola.dev", "v1", "miniousers")
    async def on_user_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is created
        """
        spec = UserSpec.model_validate(body["spec"])
        # define secret key ref namespace if omitted
        if spec.secret_key_ref.namespace is None:
            spec.secret_key_ref.namespace = operator_core.resource_namespace(body)
        user = await self.resolve_minio_user_spec(spec, body)
        await self.create_minio_user(user)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @operator_core.hook("update", "bfiola.dev", "v1", "miniousers")
    async def on_user_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = UserSpec.model_validate(body["spec"])
        # define secret key ref namespace if omitted
        if new_spec.secret_key_ref.namespace is None:
            new_spec.secret_key_ref.namespace = operator_core.resource_namespace(body)
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            user = await self.resolve_minio_user_spec(new_spec, body)
            await self.create_minio_user(user)
            patch.status["currentSpec"] = new_spec.model_dump()
            return

        current_spec = UserSpec.model_validate(current_spec)
        immutable = {("tenantRef",), ("accessKey",)}
        diff = operator_core.get_diff(current_spec, new_spec)
        diff = operator_core.filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = operator_core.apply_diff_item(current_spec, item)
            user = await self.resolve_minio_user_spec(current_spec, body)
            await self.update_minio_user(user)
            patch.status["currentSpec"] = current_spec.model_dump()

    @operator_core.hook("delete", "bfiola.dev", "v1", "miniousers")
    async def on_user_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = UserSpec.model_validate(current_spec)
            user = await self.resolve_minio_user_spec(current_spec, body)
        except Exception as e:
            return
        await self.delete_minio_user(user)

    async def resolve_minio_group_spec(
        self, group_spec: GroupSpec, body: kopf.Body
    ) -> Group:
        """
        Resolves a group spec to a group - translating references to actual values.
        """
        namespace = operator_core.resource_namespace(body)

        resource = operator_core.ResourceRef(
            api_version="bfiola.dev/v1",
            plural="miniogroups",
            name=body["metadata"]["name"],
            namespace=namespace,
        )

        tenant_ref = operator_core.ResourceRef(
            api_version="minio.min.io/v2",
            plural="tenants",
            name=group_spec.tenant_ref.name,
            namespace=group_spec.tenant_ref.namespace or namespace,
        )
        tenant = await self.get_tenant(tenant_ref)

        return Group(
            name=group_spec.name,
            resource=resource,
            tenant=tenant,
        )

    async def create_minio_group(self, group: Group):
        """
        Creates a new group given the provided group resource
        """
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

            await operator_core.run_sync(inner)

    async def update_minio_group(self, group: Group):
        """
        Updates an existing group given the provided group resource
        """
        async with self.create_minio_admin_client(group.tenant) as minio_admin_client:

            def inner():
                pass

            await operator_core.run_sync(inner)

    async def delete_minio_group(self, group: Group):
        """
        Deletes an existing group given the provided group resource
        """
        async with self.create_minio_admin_client(group.tenant) as minio_admin_client:

            def inner():
                try:
                    minio_admin_client.group_remove(group.name)
                except minio.error.MinioAdminException as e:
                    if e._code == "404":
                        return
                    raise e

            await operator_core.run_sync(inner)

    @operator_core.hook("create", "bfiola.dev", "v1", "miniogroups")
    async def on_group_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioGroup resource is created
        """
        spec = GroupSpec.model_validate(body["spec"])
        group = await self.resolve_minio_group_spec(spec, body)
        await self.create_minio_group(group)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @operator_core.hook("update", "bfiola.dev", "v1", "miniogroups")
    async def on_group_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioGroup resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = GroupSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            group = await self.resolve_minio_group_spec(new_spec, body)
            await self.create_minio_group(group)
            patch.status["currentSpec"] = new_spec.model_dump()
            return

        current_spec = GroupSpec.model_validate(current_spec)
        immutable = {("tenantRef",), ("name",)}
        diff = operator_core.get_diff(current_spec, new_spec)
        diff = operator_core.filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = operator_core.apply_diff_item(current_spec, item)
            group = await self.resolve_minio_group_spec(current_spec, body)
            await self.update_minio_group(group)
            patch.status["currentSpec"] = current_spec.model_dump()

    @operator_core.hook("delete", "bfiola.dev", "v1", "miniogroups")
    async def on_group_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioGroup resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = GroupSpec.model_validate(current_spec)
            group = await self.resolve_minio_group_spec(current_spec, body)
        except Exception as e:
            return
        await self.delete_minio_group(group)

    async def resolve_minio_group_binding_spec(
        self,
        group_binding_spec: GroupBindingSpec,
        body: kopf.Body,
    ) -> GroupBinding:
        """
        Resolves a group binding spec to a group binding - translating references to actual values.
        """
        namespace = operator_core.resource_namespace(body)

        resource = operator_core.ResourceRef(
            api_version="bfiola.dev/v1",
            plural="miniogroupbindings",
            name=body["metadata"]["name"],
            namespace=namespace,
        )

        tenant_ref = operator_core.ResourceRef(
            api_version="minio.min.io/v2",
            plural="tenants",
            name=group_binding_spec.tenant_ref.name,
            namespace=group_binding_spec.tenant_ref.namespace or namespace,
        )
        tenant = await self.get_tenant(tenant_ref)

        return GroupBinding(
            group=group_binding_spec.group,
            resource=resource,
            tenant=tenant,
            user=group_binding_spec.user,
        )

    async def create_minio_group_binding(self, group_binding: GroupBinding):
        """
        Creates a new group binding given the provided group binding resource
        """
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

            await operator_core.run_sync(inner)

    async def delete_minio_group_binding(self, group_binding: GroupBinding):
        """
        Deletes an existing group binding given the provided group binding resource
        """
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

            await operator_core.run_sync(inner)

    @operator_core.hook("create", "bfiola.dev", "v1", "miniogroupbindings")
    async def on_group_binding_create(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioGroupBinding resource is created
        """
        spec = GroupBindingSpec.model_validate(body["spec"])
        group_binding = await self.resolve_minio_group_binding_spec(spec, body)
        await self.create_minio_group_binding(group_binding)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @operator_core.hook("update", "bfiola.dev", "v1", "miniogroupbindings")
    async def on_group_binding_update(
        self, *, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioGroupBinding resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = GroupBindingSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            group_binding = await self.resolve_minio_group_binding_spec(new_spec, body)
            await self.create_minio_group_binding(group_binding)
            patch.status["currentSpec"] = new_spec.model_dump(by_alias=True)
            return

        current_spec = GroupBindingSpec.model_validate(current_spec)
        immutable: set[tuple[str, ...]] = {("tenantRef",)}
        diff = operator_core.get_diff(current_spec, new_spec)
        diff = operator_core.filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            group_binding = await self.resolve_minio_group_binding_spec(
                current_spec, body
            )
            await self.delete_minio_group_binding(group_binding)
            patch.status["currentSpec"] = None
            current_spec = operator_core.apply_diff_item(current_spec, item)
            group_binding = await self.resolve_minio_group_binding_spec(
                current_spec, body
            )
            await self.create_minio_group_binding(group_binding)
            patch.status["currentSpec"] = current_spec.model_dump(by_alias=True)

    @operator_core.hook("delete", "bfiola.dev", "v1", "miniogroupbindings")
    async def on_group_binding_delete(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioGroupBinding resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = GroupBindingSpec.model_validate(current_spec)
            group_binding = await self.resolve_minio_group_binding_spec(
                current_spec, body
            )
        except Exception as e:
            return
        await self.delete_minio_group_binding(group_binding)

    async def resolve_minio_policy_spec(
        self, policy_spec: PolicySpec, body: kopf.Body
    ) -> Policy:
        """
        Resolves a policy spec to a policy - translating references to actual values.
        """
        namespace = operator_core.resource_namespace(body)

        resource = operator_core.ResourceRef(
            api_version="bfiola.dev/v1",
            plural="miniopolicies",
            name=body["metadata"]["name"],
            namespace=namespace,
        )

        tenant_ref = operator_core.ResourceRef(
            api_version="minio.min.io/v2",
            plural="tenants",
            name=policy_spec.tenant_ref.name,
            namespace=policy_spec.tenant_ref.namespace or namespace,
        )
        tenant = await self.get_tenant(tenant_ref)

        return Policy(
            name=policy_spec.name,
            resource=resource,
            statement=policy_spec.statement,
            tenant=tenant,
            version=policy_spec.version,
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
            policy_data = policy.model_dump_json(include={"version", "statement"})
            policy_file.write_text(policy_data)
            yield policy_file

    async def create_minio_policy(self, policy: Policy):
        """
        Creates a new policy given the provided policy
        """
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

                return await operator_core.run_sync(inner)

    async def update_minio_policy(self, policy: Policy):
        """
        Updates an existing policy given the provided policy
        """
        async with self.create_minio_admin_client(policy.tenant) as minio_admin_client:
            async with self.minio_policy_file(policy) as policy_file:

                def inner():
                    minio_admin_client.policy_add(policy.name, f"{policy_file}")

                await operator_core.run_sync(inner)

    async def delete_minio_policy(self, policy: Policy):
        """
        Deletes an existing policy given the provided policy
        """
        async with self.create_minio_admin_client(policy.tenant) as minio_admin_client:

            def inner():
                minio_admin_client.policy_remove(policy.name)

            await operator_core.run_sync(inner)

    @operator_core.hook("create", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_create(self, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is created
        """
        spec = PolicySpec.model_validate(body["spec"])
        policy = await self.resolve_minio_policy_spec(spec, body)
        await self.create_minio_policy(policy)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @operator_core.hook("update", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = PolicySpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            policy = await self.resolve_minio_policy_spec(new_spec, body)
            await self.create_minio_policy(policy)
            patch.status["currentSpec"] = new_spec.model_dump(by_alias=True)
            return

        current_spec = PolicySpec.model_validate(current_spec)
        immutable = {("tenantRef",), ("name",)}
        diff = operator_core.get_diff(current_spec, new_spec)
        diff = operator_core.filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = operator_core.apply_diff_item(current_spec, item)
            policy = await self.resolve_minio_policy_spec(current_spec, body)
            await self.update_minio_policy(policy)
            patch.status["currentSpec"] = current_spec.model_dump(by_alias=True)

    @operator_core.hook("delete", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_delete(self, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = PolicySpec.model_validate(current_spec)
            policy = await self.resolve_minio_policy_spec(current_spec, body)
        except Exception as e:
            return
        await self.delete_minio_policy(policy)

    async def resolve_minio_policy_binding_spec(
        self,
        policy_binding_spec: PolicyBindingSpec,
        body: kopf.Body,
    ) -> PolicyBinding:
        """
        Resolves a policy binding spec to a policy binding - translating references to actual values.
        """
        namespace = operator_core.resource_namespace(body)

        resource = operator_core.ResourceRef(
            api_version="bfiola.dev/v1",
            plural="miniopolicybindings",
            name=body["metadata"]["name"],
            namespace=namespace,
        )

        tenant_ref = operator_core.ResourceRef(
            api_version="minio.min.io/v2",
            plural="tenants",
            name=policy_binding_spec.tenant_ref.name,
            namespace=policy_binding_spec.tenant_ref.namespace or namespace,
        )
        tenant = await self.get_tenant(tenant_ref)

        return PolicyBinding(
            group=policy_binding_spec.group,
            policy=policy_binding_spec.policy,
            resource=resource,
            tenant=tenant,
            user=policy_binding_spec.user,
        )

    async def create_minio_policy_binding(self, policy_binding: PolicyBinding):
        """
        Creates a new policy binding given the provided policy binding
        """
        async with self.create_minio_admin_client(
            policy_binding.tenant
        ) as minio_admin_client:

            def inner():
                try:
                    minio_admin_client.policy_set(
                        policy_binding.policy,
                        group=policy_binding.group,
                        user=policy_binding.user,
                    )
                except minio.error.MinioAdminException as e:
                    if e._code == "400":
                        if "policy change is already in effect" in e._body:
                            raise operator_core.OperatorError(f"policy binding exists")
                    raise e

            await operator_core.run_sync(inner)

    async def delete_minio_policy_binding(self, policy_binding: PolicyBinding):
        """
        Deletes an existing policy binding given the provided policy binding
        """
        async with self.create_minio_admin_client(
            policy_binding.tenant
        ) as minio_admin_client:

            def inner():
                try:
                    minio_admin_client.policy_unset(
                        policy_binding.policy,
                        group=policy_binding.group,
                        user=policy_binding.user,
                    )
                except minio.error.MinioAdminException as e:
                    if e._code == "400":
                        if "policy change is already in effect" in e._body:
                            return
                    raise e

            await operator_core.run_sync(inner)

    @operator_core.hook("create", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_create(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is created
        """
        spec = PolicyBindingSpec.model_validate(body["spec"])
        policy_binding = await self.resolve_minio_policy_binding_spec(spec, body)
        await self.create_minio_policy_binding(policy_binding)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @operator_core.hook("update", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_update(
        self, *, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = PolicyBindingSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            policy_binding = await self.resolve_minio_policy_binding_spec(
                new_spec, body
            )
            await self.create_minio_policy_binding(policy_binding)
            patch.status["currentSpec"] = new_spec.model_dump(by_alias=True)
            return

        current_spec = PolicyBindingSpec.model_validate(current_spec)
        immutable: set[tuple[str, ...]] = {("tenantRef",)}
        diff = operator_core.get_diff(current_spec, new_spec)
        diff = operator_core.filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            policy_binding = await self.resolve_minio_policy_binding_spec(
                current_spec, body
            )
            await self.delete_minio_policy_binding(policy_binding)
            patch.status["currentSpec"] = None
            current_spec = operator_core.apply_diff_item(current_spec, item)
            policy_binding = await self.resolve_minio_policy_binding_spec(
                current_spec, body
            )
            await self.create_minio_policy_binding(policy_binding)
            patch.status["currentSpec"] = current_spec.model_dump(by_alias=True)

    @operator_core.hook("delete", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_delete(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = PolicyBindingSpec.model_validate(current_spec)
            policy_binding = await self.resolve_minio_policy_binding_spec(
                current_spec, body
            )
        except Exception as e:
            return
        await self.delete_minio_policy_binding(policy_binding)
