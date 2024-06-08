import asyncio
import base64
import contextlib
import functools
import inspect
import io
import json
import logging
import os
import pathlib
import tempfile
from typing import Any, AsyncGenerator, Callable, Iterable, Protocol, TypeVar, cast

import dotenv
import kopf
import kopf._cogs.structs.diffs
import kubernetes
import minio
import minio.credentials.providers
import pydantic

logger = logging.getLogger(__name__)


@contextlib.asynccontextmanager
async def temporary_file(**kwargs) -> AsyncGenerator[pathlib.Path, None]:
    """
    Defines an async context manager that mimics that of `tempfile.NamedTemporaryFile`.

    Rather than return a file handle, returns a `pathlib.Path` object.
    """
    with tempfile.NamedTemporaryFile(**kwargs) as handle:
        yield pathlib.Path(handle.name)


class OperatorError(Exception):
    """
    Defines a custom exception raised by this module.

    Used primarily to help identify known errors for proper error management.

    (NOTE: see `handle_hook_exception`)
    """

    recoverable: bool

    def __init__(self, message: str, recoverable: bool = False):
        super().__init__(message)
        self.recoverable = recoverable


WrappedFn = TypeVar("WrappedFn", bound=Callable)


class HookFn(Protocol[WrappedFn]):
    """
    A callable with kopf hook/event data embedded
    """

    _hook_fn: bool
    _hook_event: str
    _hook_args: tuple[Any, ...]
    _hook_kwargs: dict[str, Any]
    __call__: WrappedFn


def hook(event: str, *args, **kwargs):
    """
    A decorator that attaches kopf hook/event data to an operator instance function
    """

    def inner(f: WrappedFn) -> HookFn[WrappedFn]:
        if not inspect.iscoroutinefunction(f):
            # only support async functions
            raise NotImplementedError()

        setattr(f, "_hook_fn", True)
        setattr(f, "_hook_event", event)
        setattr(f, "_hook_args", args)
        setattr(f, "_hook_kwargs", kwargs)

        return cast(HookFn[WrappedFn], f)

    return inner


def iter_hooks(obj: object) -> Iterable[HookFn]:
    """
    Given an object, iterates over instance members and yields functions decorated with the `hook` decorator.
    """
    for attr in dir(obj):
        val = getattr(obj, attr)
        if not callable(val):
            continue
        if not hasattr(val, "__dict__"):
            continue
        if "_hook_fn" not in val.__dict__:
            continue
        val = cast(HookFn, val)
        yield val


def resource_namespace(resource: dict | kopf.Body) -> str:
    """
    Gets the namespace attached to a resource - returns 'default' if unset.

    NOTE: Assumes 'resource' is namespaced.
    """
    return resource["metadata"].get("namespace", "default")


def resource_fqn(resource: dict | kopf.Body) -> str:
    """
    Returns a full-qualified name for a dict-like kubernetes resource.

    A 'fully-qualfied name' is <namespace>/<name>
    """
    namespace = resource_namespace(resource)
    name = resource["metadata"]["name"]
    return f"{namespace}/{name}"


def log_hook(hook: WrappedFn) -> WrappedFn:
    """
    Decorates a kopf hook/function and logs when the hook is called
    and when the hook succeeds/fails.

    Will additionally log a resource's fully-qualfiied name if found.
    """
    if not inspect.iscoroutinefunction(hook):
        raise NotImplementedError()

    @functools.wraps(hook)
    async def inner(*args, **kwargs):
        hook_name = hook.__name__
        if body := kwargs.get("body"):
            hook_name = f"{hook_name}:{resource_fqn(body)}"

        logger.info(f"{hook_name} started")
        try:
            rv = await hook(*args, **kwargs)
            logger.info(f"{hook_name} completed")
            return rv
        except Exception as e:
            logger.exception(f"{hook_name} failed", exc_info=e)
            raise e

    return cast(WrappedFn, inner)


def handle_hook_exception(hook: WrappedFn) -> WrappedFn:
    """
    kopf will retry any hooks that fail with an exception - unless a
    kopf.PermanentError is raised.

    This method decorates a kopf event/hook function and wraps known errors
    in `kopf.PermanentError` to prevent spurious retries.
    """
    if not inspect.iscoroutinefunction(hook):
        raise NotImplementedError()

    @functools.wraps(hook)
    async def inner(*args, **kwargs):
        try:
            return await hook(*args, **kwargs)
        except OperatorError as e:
            if e.recoverable:
                raise kopf.TemporaryError(str(e))
            else:
                raise kopf.PermanentError(str(e))
        except pydantic.ValidationError as e:
            raise kopf.PermanentError(str(e))

    return cast(WrappedFn, inner)


def create_kube_client(
    *, kube_config: pathlib.Path | None
) -> kubernetes.client.ApiClient:
    """
    Creates a kubernetes api client.

    Will use a `kube_config` file path to construct an api client if provided.
    Otherwise, will default to the in-cluster configuration.
    """
    config = kubernetes.client.Configuration()
    if kube_config:
        kubernetes.config.load_kube_config(
            config_file=f"{kube_config}", client_configuration=config
        )
    else:
        kubernetes.config.load_incluster_config(client_configuration=config)
    return kubernetes.client.ApiClient(config)


RunSyncRV = TypeVar("RunSyncRV")


async def run_sync(f: Callable[[], RunSyncRV]) -> RunSyncRV:
    """
    Convenience method to run sync functions within a thread pool executor
    to avoid blocking the running asyncio event loop.
    """
    return await asyncio.get_running_loop().run_in_executor(None, f)


async def list_tenant_fqns(
    kube_client: kubernetes.client.ApiClient,
) -> AsyncGenerator[str, None]:
    """
    Lists all minio tenants within the cluster (by fully-qualified name).
    """

    def inner():
        custom_objects_api = kubernetes.client.CustomObjectsApi(kube_client)
        resource_list = custom_objects_api.list_cluster_custom_object(
            "minio.min.io", "v2", "tenants"
        )
        return cast(dict, kube_client.sanitize_for_serialization(resource_list))

    resource_list = await run_sync(inner)
    for item in resource_list["items"]:
        yield resource_fqn(item)


class BaseModel(pydantic.BaseModel):
    def model_dump(self, **kwargs):
        """
        pydantic's default `model_dump` method will produce a `dict` that (sometimes) cannot
        be serialized via `json.dumps`.

        This method avoids this shortcoming by dumping the model to a string and loading
        the result via `json.loads`.
        """
        data_str = self.model_dump_json(**kwargs)
        return json.loads(data_str)

    def model_dump_json(self, **kwargs):
        """
        Calls the parent `model_dump_json` but sets different defaults.

        Sets `by_alias` to True by default - the operator is often serializing
        data to kubernetes in camelcase - represented by aliases in pydantic.
        """
        kwargs.setdefault("by_alias", True)
        return super().model_dump_json(**kwargs)


class ResourceRef(BaseModel):
    """
    Represents a reference to another kubernetes resource

    The 'namespace' field is optional - if 'None' assumed to be the current namespace
    of the containing object.
    """

    namespace: str | None = None
    name: str


class SecretKeyRef(ResourceRef):
    """
    Represents a reference to a key of a kubernetes 'Secret' resource.
    """

    key: str


class Tenant(BaseModel):
    """
    Represents a simplified data container pointing to a minio.min.io/Tenant resource.
    """

    access_key: str
    ca_bundle: str | None = None
    endpoint: str
    fqn: str
    secret_key: str
    secure: bool


class BucketSpec(BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioBucket resource.
    """

    name: str
    tenant: str


Bucket = BucketSpec


class UserSpec(BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioUser resource.
    """

    access_key: str = pydantic.Field(alias="accessKey")
    secret_key_ref: SecretKeyRef = pydantic.Field(alias="secretKeyRef")
    tenant: str


class User(BaseModel):
    """
    Represents a user with all references resolved
    """

    access_key: str
    secret_key: str
    tenant: str


class PolicyStatement(BaseModel):
    """
    Represents the spec.statement subfield of a bfiola.dev/MinioPolicy resource.
    """

    action: list[str]
    effect: str
    resource: list[str]


class PolicySpec(BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioPolicy resource.
    """

    statement: list[PolicyStatement]
    name: str
    tenant: str
    version: str


Policy = PolicySpec


class PolicyBindingSpec(BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioPolicyBinding resource.
    """

    user: str
    policy: str
    tenant: str


PolicyBinding = PolicyBindingSpec


async def get_resource_tenant(
    kube_client: kubernetes.client.ApiClient, namespace: str, name: str
) -> dict:
    """
    Async wrapper around a sync method to fetch a `Tenant` resource from kubernetes
    """

    def inner():
        custom_objects_api = kubernetes.client.CustomObjectsApi(kube_client)
        tenant = custom_objects_api.get_namespaced_custom_object(
            "minio.min.io", "v2", namespace, "tenants", name
        )
        return cast(dict, kube_client.sanitize_for_serialization(tenant))

    return await run_sync(inner)


async def get_resource_config_map(
    kube_client: kubernetes.client.ApiClient, namespace: str, name: str
) -> dict:
    """
    Async wrapper around a sync method to fetch a `ConfigMap` resource from kubernetes
    """

    def inner():
        core_api = kubernetes.client.CoreV1Api(kube_client)
        config_map = core_api.read_namespaced_config_map(name, namespace)
        return cast(dict, kube_client.sanitize_for_serialization(config_map))

    return await run_sync(inner)


async def get_resource_secret(
    kube_client: kubernetes.client.ApiClient, namespace: str, name: str
) -> dict:
    """
    Async wrapper around a sync method to fetch a `Secret` resource from kubernetes
    """

    def inner():
        core_api = kubernetes.client.CoreV1Api(kube_client)
        secret = core_api.read_namespaced_secret(name, namespace)
        return cast(dict, kube_client.sanitize_for_serialization(secret))

    return await run_sync(inner)


async def get_resource_service(
    kube_client: kubernetes.client.ApiClient, namespace: str, name: str
) -> dict:
    """
    Async wrapper around a sync method to fetch a `Service` resource from kubernetes
    """

    def inner():
        core_api = kubernetes.client.CoreV1Api(kube_client)
        service = core_api.read_namespaced_service(name, namespace)
        return cast(dict, kube_client.sanitize_for_serialization(service))

    return await run_sync(inner)


async def get_tenant(
    kube_client: kubernetes.client.ApiClient,
    tenant_fqn: str,
    endpoint_overrides: dict[str, str] | None = None,
) -> Tenant:
    """
    Builds a `Tenant` object from information fetched from
    resources within the kubernetes cluster.
    """
    endpoint_overrides = endpoint_overrides or {}
    namespace, name = tenant_fqn.split("/")

    # get tenant resource
    tenant = await get_resource_tenant(kube_client, namespace, name)

    # determine whether tenant uses http or https
    secure = tenant["spec"]["requestAutoCert"]

    # if the tenant uses https, fetch ca bundle used to verify self-signed cert
    # NOTE: this will need to eventually support other certificate sources (e.g., cert-manager)
    ca_bundle = None
    if secure:
        ca_bundle_config_map_name = "kube-root-ca.crt"
        ca_bundle_config_map = await get_resource_config_map(
            kube_client, namespace, ca_bundle_config_map_name
        )
        ca_bundle = ca_bundle_config_map["data"]["ca.crt"]

    # extract credentials from tenant secret
    configuration_secret_name = tenant["spec"]["configuration"]["name"]
    configuration_secret = await get_resource_secret(
        kube_client, namespace, configuration_secret_name
    )
    env_file = configuration_secret["data"]["config.env"]
    env_file = base64.b64decode(env_file)
    env_file = env_file.decode("utf-8")
    env_file = env_file.replace("export ", "")
    env_data = dotenv.dotenv_values(stream=io.StringIO(env_file))
    access_key = env_data["MINIO_ROOT_USER"]
    secret_key = env_data["MINIO_ROOT_PASSWORD"]
    if not access_key or not secret_key:
        raise OperatorError(
            f"credentials not found in secret: {resource_fqn(configuration_secret)}"
        )

    # determine endpoint
    # (NOTE: assumes service name 'minio' from helm templates)
    service_name = "minio"
    service = await get_resource_service(kube_client, namespace, service_name)
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
        raise OperatorError(f"port not found in service: {namespace}/{service_name}")
    endpoint = f"{service_name}.{namespace}.svc.cluster.local:{service_port}"
    if tenant_fqn in endpoint_overrides:
        endpoint = endpoint_overrides[tenant_fqn]

    return Tenant(
        access_key=access_key,
        ca_bundle=ca_bundle,
        endpoint=endpoint,
        fqn=tenant_fqn,
        secret_key=secret_key,
        secure=secure,
    )


@contextlib.asynccontextmanager
async def create_minio_client(tenant: Tenant) -> AsyncGenerator[minio.Minio, None]:
    """
    Creates a minio client from the given tenant.
    """
    async with temporary_file(suffix=".crt") as ca_cert:
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
    tenant: Tenant,
) -> AsyncGenerator[minio.MinioAdmin, None]:
    """
    Creates a minio admin client from the given tenant
    """
    async with temporary_file(suffix=".crt") as ca_cert:
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


async def resolve_minio_bucket_spec(
    kube_client: kubernetes.client.ApiClient, bucket_spec: BucketSpec
) -> Bucket:
    """
    Resolves a bucket spec to a bucket - translating references to actual values.
    """
    return bucket_spec


async def create_minio_bucket(tenant: Tenant, bucket: Bucket):
    """
    Creates a new minio bucket given the provided bucket
    """
    async with create_minio_client(tenant) as minio_client:

        def inner():
            try:
                minio_client.make_bucket(bucket.name)
            except minio.error.S3Error as e:
                if e.code == "BucketAlreadyOwnedByYou":
                    raise OperatorError(
                        f"bucket already exists: {tenant.fqn}/{bucket.name}"
                    )
                raise e

        await run_sync(inner)


async def update_minio_bucket(tenant: Tenant, bucket: Bucket):
    """
    Updates an existing minio bucket given the provided bucket
    """
    async with create_minio_client(tenant) as minio_client:

        def inner():
            pass

        await run_sync(inner)


async def delete_minio_bucket(tenant: Tenant, bucket: Bucket):
    """
    Deletes an existing minio bucket given the provided bucket
    """
    async with create_minio_client(tenant) as minio_client:

        def inner():
            try:
                minio_client.remove_bucket(bucket.name)
            except minio.error.S3Error as e:
                if e.code == "NoSuchBucket":
                    return
                raise e

        await run_sync(inner)


async def resolve_minio_user_spec(
    kube_client: kubernetes.client.ApiClient, user_spec: UserSpec
) -> User:
    """
    Resolves a user spec to a user - translating references to actual values.
    """
    # NOTE: the operator should set this value if it's omitted from the actual resource.
    if user_spec.secret_key_ref.namespace is None:
        raise OperatorError(f"unknown namespace for secret key ref")
    secret = await get_resource_secret(
        kube_client, user_spec.secret_key_ref.namespace, user_spec.secret_key_ref.name
    )
    secret_key = secret["data"][user_spec.secret_key_ref.key]
    secret_key = base64.b64decode(secret_key)
    secret_key = secret_key.decode("utf-8")
    return User(
        access_key=user_spec.access_key, secret_key=secret_key, tenant=user_spec.tenant
    )


async def create_minio_user(tenant: Tenant, user: User):
    """
    Creates a new user given the provided user
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:

        def inner():
            # NOTE: the `user_add` endpoint will succeed even if a user with the access key already exists
            try:
                minio_admin_client.user_info(user.access_key)
                raise OperatorError(f"user already exists: {user.access_key}")
            except minio.error.MinioAdminException as e:
                if e._code != "404":
                    raise e

            minio_admin_client.user_add(user.access_key, user.secret_key)

        await run_sync(inner)


async def update_minio_user(tenant: Tenant, user: User):
    """
    Updates an existing user given the provided user
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:

        def inner():
            minio_admin_client.user_add(user.access_key, user.secret_key)

        await run_sync(inner)


async def delete_minio_user(tenant: Tenant, user: User):
    """
    Deletes an existing user given the provided user
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:

        def inner():
            try:
                minio_admin_client.user_remove(user.access_key)
            except minio.error.MinioAdminException as e:
                if e._code == "404":
                    return
                raise e

        await run_sync(inner)


async def resolve_minio_policy_spec(
    kube_client: kubernetes.client.ApiClient, policy_spec: PolicySpec
) -> Policy:
    """
    Resolves a policy spec to a policy - translating references to actual values.
    """
    return policy_spec


@contextlib.asynccontextmanager
async def minio_policy_file(
    policy: Policy,
) -> AsyncGenerator[pathlib.Path, None]:
    """
    Provides a context that writes a given policy to a policy file suitable for use with the minio admin apis.
    """
    async with temporary_file(suffix=".json") as policy_file:
        policy_data = policy.model_dump_json(include={"version", "statement"})
        policy_file.write_text(policy_data)
        yield policy_file


async def create_minio_policy(tenant: Tenant, policy: Policy):
    """
    Creates a new policy given the provided policy
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:
        async with minio_policy_file(policy) as policy_file:

            def inner():
                # NOTE: the `policy_add` endpoint will succeed even if a policy with the given name already exists
                try:
                    minio_admin_client.policy_info(policy.name)
                    raise OperatorError(f"policy already exists: {policy.name}")
                except minio.error.MinioAdminException as e:
                    if e._code != "404":
                        raise e

                minio_admin_client.policy_add(policy.name, f"{policy_file}")

            return await run_sync(inner)


async def update_minio_policy(tenant: Tenant, policy: Policy):
    """
    Updates an existing policy given the provided policy
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:
        async with minio_policy_file(policy) as policy_file:

            def inner():
                minio_admin_client.policy_add(policy.name, f"{policy_file}")

            await run_sync(inner)


async def delete_minio_policy(tenant: Tenant, policy: Policy):
    """
    Deletes an existing policy given the provided policy
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:

        def inner():
            minio_admin_client.policy_remove(policy.name)

        await run_sync(inner)


async def resolve_minio_policy_binding_spec(
    kube_client: kubernetes.client.ApiClient, policy_binding_spec: PolicyBindingSpec
) -> PolicyBinding:
    """
    Resolves a policy binding spec to a policy binding - translating references to actual values.
    """
    return policy_binding_spec


async def create_minio_policy_binding(tenant: Tenant, policy_binding: PolicyBinding):
    """
    Creates a new policy binding given the provided policy binding
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:

        def inner():
            try:
                minio_admin_client.policy_set(
                    policy_binding.policy, user=policy_binding.user
                )
            except minio.error.MinioAdminException as e:
                if e._code == "400":
                    if "policy change is already in effect" in e._body:
                        raise OperatorError(f"policy binding exists")
                raise e

        await run_sync(inner)


async def delete_minio_policy_binding(tenant: Tenant, policy_binding: PolicyBinding):
    """
    Deletes an existing policy binding given the provided policy binding
    """
    async with create_minio_admin_client(tenant) as minio_admin_client:

        def inner():
            try:
                minio_admin_client.policy_unset(
                    policy_binding.policy, user=policy_binding.user
                )
            except minio.error.MinioAdminException as e:
                if e._code == "400":
                    if "policy change is already in effect" in e._body:
                        return
                raise e

        await run_sync(inner)


SomeModel = TypeVar("SomeModel", bound=BaseModel)


def get_diff(a: SomeModel, b: SomeModel) -> kopf.Diff:
    """
    Helper method to return a diff between two models of the same class.
    """
    return kopf._cogs.structs.diffs.diff(a.model_dump(), b.model_dump())


def apply_diff_item(model: SomeModel, item: kopf.DiffItem) -> SomeModel:
    """
    Applies a given diff item to an model - returning an updated
    copy of the model.
    """
    data = model.model_dump()
    operation, field, old_value, new_value = item
    if operation == "change":
        curr = data
        # traverse object parent fields
        for f in field[:-1]:
            curr = data[f]
        # set final field value
        field = field[-1]
        curr[field] = new_value
    else:
        raise NotImplementedError()
    return type(model).model_validate(data)


def filter_immutable_diff_items(
    diff: kopf.Diff, immutable: set[tuple[str, ...]], kopf_logger: logging.Logger
) -> Iterable[kopf.DiffItem]:
    """
    Most resources have fields that shouldn't change during updates - and will often
    need to filter out diff items that attempt to modify existing fields.

    This helper function will yield diff items that aren't part of the provided immutable
    fields set.
    """
    for item in diff:
        if item[1] in immutable:
            kopf_logger.info(f"ignoring immutable field: {item[1]}")
            continue
        yield item


class Operator:
    """
    Implements a kubernetes operator capable of syncing minio tenants and
    a handful of custom resource definitions with a remote minio server.
    """

    # allows the caller to override endpoints in this tenant_fqn -> endpoint mapping
    # (this is useful for out-of-cluster development where a service DNS record isn't connectable)
    endpoint_overrides: dict[str, str]
    # a client capable of communcating with kubernetes
    kube_client: kubernetes.client.ApiClient
    # an (optional) path to a kubeconfig file
    kube_config: pathlib.Path | None
    # a kopf.OperatorRegistry instance enabling this operator to *not* run in the module scope
    registry: kopf.OperatorRegistry

    def __init__(
        self,
        *,
        endpoint_overrides: dict[str, str] | None = None,
        kube_config: pathlib.Path | None = None,
    ):
        self.endpoint_overrides = endpoint_overrides or {}
        self.kube_client = cast(kubernetes.client.ApiClient, None)
        self.kube_config = kube_config
        self.registry = kopf.OperatorRegistry()

        # register operator hooks
        for hook in iter_hooks(self):
            kopf_decorator_fn = getattr(kopf.on, hook._hook_event)
            kopf_decorator = kopf_decorator_fn(
                *hook._hook_args, registry=self.registry, **hook._hook_kwargs
            )
            hook = log_hook(hook)
            hook = handle_hook_exception(hook)
            kopf_decorator(hook)

    async def get_tenant(self, tenant_fqn: str) -> Tenant:
        """
        Helper method that calls `get_tenant` - providing common operator-level args.
        """
        return await get_tenant(
            self.kube_client, tenant_fqn, endpoint_overrides=self.endpoint_overrides
        )

    @hook("startup")
    async def startup(self, **kwargs):
        """
        Initializes the operator
        """
        self.kube_client = create_kube_client(kube_config=self.kube_config)

    @hook("login")
    async def login(self, **kwargs):
        """
        Authenticates the operator with kubernetes
        """
        if self.kube_config:
            logger.debug(f"using kubeconfig: {self.kube_config}")
            env = os.environ
            try:
                os.environ = dict(os.environ)
                os.environ["KUBECONFIG"] = f"{self.kube_config}"
                return kopf.login_with_kubeconfig()
            finally:
                os.environ = env
        else:
            logger.debug(f"using in-cluster")
            return kopf.login_with_service_account()

    @hook("create", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is created
        """
        spec = BucketSpec.model_validate(body["spec"])
        tenant = await self.get_tenant(spec.tenant)
        bucket = await resolve_minio_bucket_spec(self.kube_client, spec)
        await create_minio_bucket(tenant, bucket)
        patch.status["currentSpec"] = spec.model_dump()

    @hook("update", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = BucketSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            tenant = await self.get_tenant(new_spec.tenant)
            bucket = await resolve_minio_bucket_spec(self.kube_client, new_spec)
            await create_minio_bucket(tenant, bucket)
            patch.status["currentSpec"] = new_spec.model_dump()
            return

        current_spec = BucketSpec.model_validate(current_spec)
        tenant = await self.get_tenant(current_spec.tenant)
        immutable = {("tenant",), ("name",)}
        diff = get_diff(current_spec, new_spec)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = apply_diff_item(current_spec, item)
            bucket = await resolve_minio_bucket_spec(self.kube_client, current_spec)
            await update_minio_bucket(tenant, bucket)
            patch.status["currentSpec"] = current_spec.model_dump()

    @hook("delete", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = BucketSpec.model_validate(current_spec)
            tenant = await self.get_tenant(current_spec.tenant)
            bucket = await resolve_minio_bucket_spec(self.kube_client, current_spec)
        except Exception as e:
            return
        await delete_minio_bucket(tenant, bucket)

    @hook("create", "bfiola.dev", "v1", "miniousers")
    async def on_user_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is created
        """
        spec = UserSpec.model_validate(body["spec"])
        tenant = await self.get_tenant(spec.tenant)
        # define secret key ref namespace if omitted
        if spec.secret_key_ref.namespace is None:
            spec.secret_key_ref.namespace = resource_namespace(body)
        user = await resolve_minio_user_spec(self.kube_client, spec)
        await create_minio_user(tenant, user)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @hook("update", "bfiola.dev", "v1", "miniousers")
    async def on_user_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = UserSpec.model_validate(body["spec"])
        # define secret key ref namespace if omitted
        if new_spec.secret_key_ref.namespace is None:
            new_spec.secret_key_ref.namespace = resource_namespace(body)
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            tenant = await self.get_tenant(new_spec.tenant)
            user = await resolve_minio_user_spec(self.kube_client, new_spec)
            await create_minio_user(tenant, user)
            patch.status["currentSpec"] = new_spec.model_dump()
            return

        current_spec = UserSpec.model_validate(current_spec)
        tenant = await self.get_tenant(current_spec.tenant)
        immutable = {("tenant",), ("accessKey",)}
        diff = get_diff(current_spec, new_spec)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = apply_diff_item(current_spec, item)
            user = await resolve_minio_user_spec(self.kube_client, current_spec)
            await update_minio_user(tenant, user)
            patch.status["currentSpec"] = current_spec.model_dump()

    @hook("delete", "bfiola.dev", "v1", "miniousers")
    async def on_user_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = UserSpec.model_validate(current_spec)
            tenant = await self.get_tenant(current_spec.tenant)
            user = await resolve_minio_user_spec(self.kube_client, current_spec)
        except Exception as e:
            return
        await delete_minio_user(tenant, user)

    @hook("create", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_create(self, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is created
        """
        spec = PolicySpec.model_validate(body["spec"])
        tenant = await self.get_tenant(spec.tenant)
        policy = await resolve_minio_policy_spec(self.kube_client, spec)
        await create_minio_policy(tenant, policy)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @hook("update", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new_spec = PolicySpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")

        # handle updates to resources that previously failed to create
        if not current_spec:
            tenant = await self.get_tenant(new_spec.tenant)
            policy = await resolve_minio_policy_spec(self.kube_client, new_spec)
            await create_minio_policy(tenant, policy)
            patch.status["currentSpec"] = new_spec.model_dump(by_alias=True)
            return

        current_spec = PolicySpec.model_validate(current_spec)
        tenant = await self.get_tenant(current_spec.tenant)
        immutable = {("tenant",), ("name",)}
        diff = get_diff(current_spec, new_spec)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current_spec = apply_diff_item(current_spec, item)
            policy = await resolve_minio_policy_spec(self.kube_client, current_spec)
            await update_minio_policy(tenant, policy)
            patch.status["currentSpec"] = current_spec.model_dump(by_alias=True)

    @hook("delete", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_delete(self, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = PolicySpec.model_validate(current_spec)
            tenant = await self.get_tenant(current_spec.tenant)
            policy = await resolve_minio_policy_spec(self.kube_client, current_spec)
        except Exception as e:
            return
        await delete_minio_policy(tenant, policy)

    @hook("create", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_create(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is created
        """
        spec = PolicyBindingSpec.model_validate(body["spec"])
        tenant = await self.get_tenant(spec.tenant)
        policy_binding = await resolve_minio_policy_binding_spec(self.kube_client, spec)
        await create_minio_policy_binding(tenant, policy_binding)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @hook("update", "bfiola.dev", "v1", "miniopolicybindings")
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
            tenant = await self.get_tenant(new_spec.tenant)
            policy_binding = await resolve_minio_policy_binding_spec(
                self.kube_client, new_spec
            )
            await create_minio_policy_binding(tenant, policy_binding)
            patch.status["currentSpec"] = new_spec.model_dump(by_alias=True)
            return

        current_spec = PolicyBindingSpec.model_validate(current_spec)
        tenant = await self.get_tenant(current_spec.tenant)
        immutable: set[tuple[str, ...]] = {("tenant",)}
        diff = get_diff(current_spec, new_spec)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            policy_binding = await resolve_minio_policy_binding_spec(
                self.kube_client, current_spec
            )
            await delete_minio_policy_binding(tenant, policy_binding)
            patch.status["currentSpec"] = None
            current_spec = apply_diff_item(current_spec, item)
            policy_binding = await resolve_minio_policy_binding_spec(
                self.kube_client, current_spec
            )
            await create_minio_policy_binding(tenant, policy_binding)
            patch.status["currentSpec"] = current_spec.model_dump(by_alias=True)

    @hook("delete", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_delete(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            current_spec = PolicyBindingSpec.model_validate(current_spec)
            tenant = await self.get_tenant(current_spec.tenant)
            policy_binding = await resolve_minio_policy_binding_spec(
                self.kube_client, current_spec
            )
        except Exception as e:
            return
        await delete_minio_policy_binding(tenant, policy_binding)

    async def run(self):
        """
        Runs the operator - and blocks until exit.
        """
        await kopf.operator(clusterwide=True, registry=self.registry)
