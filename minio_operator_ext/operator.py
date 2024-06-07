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
    Defines a custom exception raised by this module - usually an error that is not-recoverable.

    Used primarily to help identify known errors for proper error management.

    (NOTE: see `handle_hook_exception`)
    """

    pass


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


def resource_fqn(resource: dict | kopf.Body) -> str:
    """
    Returns a full-qualified name for a dict-like kubernetes resource.

    A 'fully-qualfied name' is <namespace>/<name>
    """
    namespace = resource["metadata"]["namespace"]
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


class Tenant(BaseModel):
    """
    Represents a simplified data container pointing to a minio.min.io/Tenant resource.
    """

    access_key: str
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


class UserSpec(BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioUser resource.
    """

    access_key: str = pydantic.Field(alias="accessKey")
    secret_key: str = pydantic.Field(alias="secretKey")
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


class PolicyBindingSpec(BaseModel):
    """
    Represents the spec field of a bfiola.dev/MinioPolicyBinding resource.
    """

    user: str
    policy: str
    tenant: str


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
    def _get_tenant():
        custom_objects_api = kubernetes.client.CustomObjectsApi(kube_client)
        tenant = custom_objects_api.get_namespaced_custom_object(
            "minio.min.io", "v2", namespace, "tenants", name
        )
        return cast(dict, kube_client.sanitize_for_serialization(tenant))

    tenant = await run_sync(_get_tenant)

    # determine whether tenant uses http or https
    secure = tenant["spec"]["requestAutoCert"]

    # extract credentials from tenant secret
    secret_name = tenant["spec"]["configuration"]["name"]

    def _get_secret():
        core_api = kubernetes.client.CoreV1Api(kube_client)
        secret = core_api.read_namespaced_secret(secret_name, namespace)
        return cast(dict, kube_client.sanitize_for_serialization(secret))

    secret = await run_sync(_get_secret)
    env_file = base64.b64decode(secret["data"]["config.env"]).decode("utf-8")
    env_file = env_file.replace("export ", "")
    env_data = dotenv.dotenv_values(stream=io.StringIO(env_file))
    access_key = env_data["MINIO_ROOT_USER"]
    secret_key = env_data["MINIO_ROOT_PASSWORD"]
    if not access_key or not secret_key:
        raise OperatorError(
            f"credentials not found in secret: {namespace}/{secret_name}"
        )

    # determine endpoint
    # (NOTE: assumes service name 'minio' from helm templates)
    service_name = "minio"

    def _get_service():
        core_api = kubernetes.client.CoreV1Api(kube_client)
        service = core_api.read_namespaced_service(service_name, namespace)
        return cast(dict, kube_client.sanitize_for_serialization(service))

    service = await run_sync(_get_service)
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
        endpoint=endpoint,
        fqn=tenant_fqn,
        secret_key=secret_key,
        secure=secure,
    )


def create_minio_client(tenant: Tenant) -> minio.Minio:
    """
    Creates a minio client from the given tenant.
    """
    return minio.Minio(
        access_key=tenant.access_key,
        endpoint=tenant.endpoint,
        secret_key=tenant.secret_key,
        secure=tenant.secure,
    )


def create_minio_admin_client(tenant: Tenant) -> minio.MinioAdmin:
    """
    Creates a minio admin client from the given tenant
    """
    return minio.MinioAdmin(
        credentials=minio.credentials.providers.StaticProvider(
            access_key=tenant.access_key, secret_key=tenant.secret_key
        ),
        endpoint=tenant.endpoint,
        secure=tenant.secure,
    )


async def create_minio_bucket(tenant: Tenant, bucket_spec: BucketSpec):
    """
    Creates a new minio bucket given the provided bucket spec
    """
    minio_client = create_minio_client(tenant)
    try:
        minio_client.make_bucket(bucket_spec.name)
    except minio.error.S3Error as e:
        if e.code == "BucketAlreadyOwnedByYou":
            raise OperatorError(
                f"bucket already exists: {tenant.fqn}/{bucket_spec.name}"
            )
        raise e


async def update_minio_bucket(tenant: Tenant, bucket_spec: BucketSpec):
    """
    Updates an existing minio bucket given the provided bucket spec
    """
    minio_client = create_minio_client(tenant)


async def delete_minio_bucket(tenant: Tenant, bucket_spec: BucketSpec):
    """
    Deletes an existing minio bucket given the provided bucket spec
    """
    minio_client = create_minio_client(tenant)
    try:
        minio_client.remove_bucket(bucket_spec.name)
    except minio.error.S3Error as e:
        if e.code == "NoSuchBucket":
            return
        raise e


async def create_minio_user(tenant: Tenant, user_spec: UserSpec):
    """
    Creates a new user given the provided user spec
    """
    minio_admin_client = create_minio_admin_client(tenant)

    # NOTE: the `user_add` endpoint will succeed even if a user with the access key already exists
    try:
        minio_admin_client.user_info(user_spec.access_key)
        raise OperatorError(f"user already exists: {user_spec.access_key}")
    except minio.error.MinioAdminException as e:
        if e._code != "404":
            raise e

    minio_admin_client.user_add(user_spec.access_key, user_spec.secret_key)


async def update_minio_user(tenant: Tenant, user_spec: UserSpec):
    """
    Updates an existing user given the provided user spec
    """
    minio_admin_client = create_minio_admin_client(tenant)

    minio_admin_client.user_add(user_spec.access_key, user_spec.secret_key)


async def delete_minio_user(tenant: Tenant, user_spec: UserSpec):
    """
    Deletes an existing user given the provided user spec
    """
    minio_admin_client = create_minio_admin_client(tenant)
    try:
        minio_admin_client.user_remove(user_spec.access_key)
    except minio.error.MinioAdminException as e:
        if e._code == "404":
            return
        raise e


@contextlib.asynccontextmanager
async def minio_policy_file(
    policy_spec: PolicySpec,
) -> AsyncGenerator[pathlib.Path, None]:
    """
    Provides a context that writes a given policy spec to a policy file suitable for use with the minio admin apis.
    """
    async with temporary_file(suffix=".json") as policy_file:
        policy_data = policy_spec.model_dump_json(include={"version", "statement"})
        policy_file.write_text(policy_data)
        yield policy_file


async def create_minio_policy(tenant: Tenant, policy_spec: PolicySpec):
    """
    Creates a new policy given the provided policy spec
    """
    minio_admin_client = create_minio_admin_client(tenant)

    # NOTE: the `policy_add` endpoint will succeed even if a policy with the given name already exists
    try:
        minio_admin_client.policy_info(policy_spec.name)
        raise OperatorError(f"policy already exists: {policy_spec.name}")
    except minio.error.MinioAdminException as e:
        if e._code != "404":
            raise e

    async with minio_policy_file(policy_spec) as policy_file:
        minio_admin_client.policy_add(policy_spec.name, f"{policy_file}")


async def update_minio_policy(tenant: Tenant, policy_spec: PolicySpec):
    """
    Updates an existing policy given the provided policy spec
    """
    minio_admin_client = create_minio_admin_client(tenant)

    async with minio_policy_file(policy_spec) as policy_file:
        minio_admin_client.policy_add(policy_spec.name, f"{policy_file}")


async def delete_minio_policy(tenant: Tenant, policy_spec: PolicySpec):
    """
    Deletes an existing policy given the provided policy spec
    """
    minio_admin_client = create_minio_admin_client(tenant)

    minio_admin_client.policy_remove(policy_spec.name)


async def create_minio_policy_binding(
    tenant: Tenant, policy_binding_spec: PolicyBindingSpec
):
    """
    Creates a new policy binding given the provided policy binding spec
    """
    minio_admin_client = create_minio_admin_client(tenant)
    try:
        minio_admin_client.policy_set(
            policy_binding_spec.policy, user=policy_binding_spec.user
        )
    except minio.error.MinioAdminException as e:
        if e._code == "400":
            if "policy change is already in effect" in e._body:
                raise OperatorError(f"policy binding exists")
        if e._code == "404":
            if "user does not exist" in e._body:
                raise OperatorError(f"user does not exist: {policy_binding_spec.user}")
            elif "canned policy does not exist" in e._body:
                raise OperatorError(
                    f"policy does not exist: {policy_binding_spec.policy}"
                )
        raise e


async def delete_minio_policy_binding(
    tenant: Tenant, policy_binding_spec: PolicyBindingSpec
):
    """
    Deletes an existing policy binding given the provided policy binding spec
    """
    minio_admin_client = create_minio_admin_client(tenant)
    try:
        minio_admin_client.policy_unset(
            policy_binding_spec.policy, user=policy_binding_spec.user
        )
    except minio.error.MinioAdminException as e:
        if e._code == "400":
            if "policy change is already in effect" in e._body:
                return
        raise e


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
        data[field[0]] = new_value
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
    # a tenant_fqn -> Tenant map used to process crd crud operations
    tenants: dict[str, Tenant]

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
        self.tenants = {}

        # register operator hooks
        for hook in iter_hooks(self):
            kopf_decorator_fn = getattr(kopf.on, hook._hook_event)
            kopf_decorator = kopf_decorator_fn(
                *hook._hook_args, registry=self.registry, **hook._hook_kwargs
            )
            hook = log_hook(hook)
            hook = handle_hook_exception(hook)
            kopf_decorator(hook)

    @hook("startup")
    async def startup(self, **kwargs):
        """
        Initializes the operator
        """
        self.kube_client = create_kube_client(kube_config=self.kube_config)

        async for tenant_fqn in list_tenant_fqns(self.kube_client):
            self.tenants[tenant_fqn] = await get_tenant(
                self.kube_client, tenant_fqn, endpoint_overrides=self.endpoint_overrides
            )

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

    @hook("create", "minio.min.io", "v2", "tenant")
    async def on_tenant_create(self, *, body: kopf.Body, **kwargs):
        """
        Called when a minio.min.io/v2/Tenant resource is created
        """
        tenant_fqn = resource_fqn(body)
        self.tenants[tenant_fqn] = await get_tenant(
            self.kube_client, tenant_fqn, endpoint_overrides=self.endpoint_overrides
        )

    @hook("update", "minio.min.io", "v2", "tenant")
    async def on_tenant_update(self, *, body: kopf.Body, **kwargs):
        """
        Called when a minio.min.io/v2/Tenant resource is updated
        """
        tenant_fqn = resource_fqn(body)
        self.tenants[tenant_fqn] = await get_tenant(
            self.kube_client, tenant_fqn, endpoint_overrides=self.endpoint_overrides
        )

    @hook("delete", "minio.min.io", "v2", "tenant")
    async def on_tenant_delete(self, *, body: kopf.Body, **kwargs):
        """
        Called when a minio.min.io/v2/Tenant resource is deleted
        """
        tenant_fqn = resource_fqn(body)
        self.tenants.pop(tenant_fqn, None)

    @hook("create", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is created
        """
        spec = BucketSpec.model_validate(body["spec"])
        tenant = self.tenants[spec.tenant]
        await create_minio_bucket(tenant, spec)
        patch.status["currentSpec"] = spec.model_dump()

    @hook("update", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new = BucketSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")
        if not current_spec:
            tenant = self.tenants[new.tenant]
            await create_minio_bucket(tenant, new)
            patch.status["currentSpec"] = new.model_dump()
            return
        current = BucketSpec.model_validate(current_spec)
        tenant = self.tenants[current.tenant]

        immutable = {("tenant",), ("name",)}
        diff = get_diff(current, new)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current = apply_diff_item(current, item)
            await update_minio_bucket(tenant, current)
            patch.status["currentSpec"] = current.model_dump()

    @hook("delete", "bfiola.dev", "v1", "miniobuckets")
    async def on_bucket_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioBucket resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            spec = BucketSpec.model_validate(current_spec)
            tenant = self.tenants[spec.tenant]
        except Exception as e:
            return
        await delete_minio_bucket(tenant, spec)

    @hook("create", "bfiola.dev", "v1", "miniousers")
    async def on_user_create(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is created
        """
        spec = UserSpec.model_validate(body["spec"])
        tenant = self.tenants[spec.tenant]
        await create_minio_user(tenant, spec)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @hook("update", "bfiola.dev", "v1", "miniousers")
    async def on_user_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new = UserSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")
        if not current_spec:
            tenant = self.tenants[new.tenant]
            await create_minio_user(tenant, new)
            patch.status["currentSpec"] = new.model_dump()
            return
        current = UserSpec.model_validate(current_spec)
        tenant = self.tenants[current.tenant]

        immutable = {("tenant",), ("accessKey",)}
        diff = get_diff(current, new)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current = apply_diff_item(current, item)
            await update_minio_user(tenant, current)
            patch.status["currentSpec"] = current.model_dump()

    @hook("delete", "bfiola.dev", "v1", "miniousers")
    async def on_user_delete(self, body: kopf.Body, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioUser resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            spec = UserSpec.model_validate(current_spec)
            tenant = self.tenants[spec.tenant]
        except Exception as e:
            return
        await delete_minio_user(tenant, spec)

    @hook("create", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_create(self, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is created
        """
        spec = PolicySpec.model_validate(body["spec"])
        tenant = self.tenants[spec.tenant]
        await create_minio_policy(tenant, spec)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @hook("update", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_update(self, *, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new = PolicySpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")
        if not current_spec:
            tenant = self.tenants[new.tenant]
            await create_minio_policy(tenant, new)
            patch.status["currentSpec"] = new.model_dump(by_alias=True)
            return
        current = PolicySpec.model_validate(current_spec)
        tenant = self.tenants[current.tenant]

        immutable = {("tenant",), ("name",)}
        diff = get_diff(current, new)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            current = apply_diff_item(current, item)
            await update_minio_policy(tenant, current)
            patch.status["currentSpec"] = current.model_dump(by_alias=True)

    @hook("delete", "bfiola.dev", "v1", "miniopolicies")
    async def on_policy_delete(self, body: kopf.Body, patch: kopf.Patch, **kwargs):
        """
        Called when a bfiola.dev/v1/MinioPolicy resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            spec = PolicySpec.model_validate(current_spec)
            tenant = self.tenants[spec.tenant]
        except Exception as e:
            return
        await delete_minio_policy(tenant, spec)

    @hook("create", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_create(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is created
        """
        spec = PolicyBindingSpec.model_validate(body["spec"])
        tenant = self.tenants[spec.tenant]
        await create_minio_policy_binding(tenant, spec)
        patch.status["currentSpec"] = spec.model_dump(by_alias=True)

    @hook("update", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_update(
        self, *, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is updated
        """
        kopf_logger: logging.Logger = kwargs["logger"]
        new = PolicyBindingSpec.model_validate(body["spec"])
        current_spec = body["status"].get("currentSpec")
        if not current_spec:
            tenant = self.tenants[new.tenant]
            await create_minio_policy_binding(tenant, new)
            patch.status["currentSpec"] = new.model_dump(by_alias=True)
            return
        current = PolicyBindingSpec.model_validate(current_spec)
        tenant = self.tenants[current.tenant]

        immutable: set[tuple[str, ...]] = {("tenant",)}
        diff = get_diff(current, new)
        diff = filter_immutable_diff_items(diff, immutable, kopf_logger)
        for item in diff:
            await delete_minio_policy_binding(tenant, current)
            patch.status["currentSpec"] = None
            current = apply_diff_item(current, item)
            await create_minio_policy_binding(tenant, current)
            patch.status["currentSpec"] = current.model_dump(by_alias=True)

    @hook("delete", "bfiola.dev", "v1", "miniopolicybindings")
    async def on_policy_binding_delete(
        self, body: kopf.Body, patch: kopf.Patch, **kwargs
    ):
        """
        Called when a bfiola.dev/v1/MinioPolicyBinding resource is deleted
        """
        try:
            current_spec = body["status"]["currentSpec"]
            spec = PolicyBindingSpec.model_validate(current_spec)
            tenant = self.tenants[spec.tenant]
        except Exception as e:
            return
        await delete_minio_policy_binding(tenant, spec)

    async def run(self):
        """
        Runs the operator - and blocks until exit.
        """
        await kopf.operator(clusterwide=True, registry=self.registry)
