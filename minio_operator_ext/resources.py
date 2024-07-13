from typing import Any

import operator_core
import pydantic


class TenantRef(pydantic.BaseModel):
    name: str
    namespace: str | None = None


class SecretRef(pydantic.BaseModel):
    key: str
    name: str
    namespace: str | None = None


class MinioBucketSpec(pydantic.BaseModel):
    name: str
    tenantRef: TenantRef


class MinioBucket(operator_core.NamespacedResource[MinioBucketSpec]):
    __oc_resource__ = {
        "api_version": "bfiola.dev/v1",
        "kind": "MinioBucket",
        "plural": "miniobuckets",
    }
    __oc_immutable_fields__ = {("name",), ("tenantRef",)}


class MinioUserSpec(pydantic.BaseModel):
    accessKey: str
    secretKeyRef: SecretRef
    tenantRef: TenantRef


class MinioUser(operator_core.NamespacedResource[MinioUserSpec]):
    __oc_resource__ = {
        "api_version": "bfiola.dev/v1",
        "kind": "MinioUser",
        "plural": "miniousers",
    }
    __oc_immutable_fields__ = {("accessKey",), ("tenantRef",)}


class MinioGroupSpec(pydantic.BaseModel):
    name: str
    tenantRef: TenantRef


class MinioGroup(operator_core.NamespacedResource[MinioGroupSpec]):
    __oc_resource__ = {
        "api_version": "bfiola.dev/v1",
        "kind": "MinioGroup",
        "plural": "miniogroups",
    }
    __oc_immutable_fields__ = {("name",), ("tenantRef",)}


class MinioGroupBindingSpec(pydantic.BaseModel):
    group: str
    tenantRef: TenantRef
    user: str


class MinioGroupBinding(operator_core.NamespacedResource[MinioGroupBindingSpec]):
    __oc_resource__ = {
        "api_version": "bfiola.dev/v1",
        "kind": "MinioGroupBinding",
        "plural": "miniogroupbindings",
    }
    __oc_immutable_fields__ = {("group",), ("tenantRef",), ("user",)}


class PolicyStatement(pydantic.BaseModel):
    action: list[str]
    effect: str
    resource: list[str]


class MinioPolicySpec(pydantic.BaseModel):
    name: str
    statement: list[PolicyStatement]
    tenantRef: TenantRef
    version: str


class MinioPolicy(operator_core.NamespacedResource[MinioPolicySpec]):
    __oc_resource__ = {
        "api_version": "bfiola.dev/v1",
        "kind": "MinioPolicy",
        "plural": "miniopolicies",
    }
    __oc_immutable_fields__ = {("name",), ("tenantRef",)}


class MinioPolicyIdentity(pydantic.BaseModel):
    builtin: str | None = None
    ldap: str | None = None

    @pydantic.model_validator(mode="before")
    @classmethod
    def one_of_builtin_or_ldap(cls, data: Any) -> Any:
        builtin = data.get("builtin")
        ldap = data.get("ldap")
        if (builtin is not None) == (ldap is not None):
            raise ValueError(f"only one of [builtin, ldap] must be defined")
        return data


class MinioPolicyBindingSpec(pydantic.BaseModel):
    policy: str
    tenantRef: TenantRef
    group: MinioPolicyIdentity | None = None
    user: MinioPolicyIdentity | None = None

    @pydantic.model_validator(mode="before")
    @classmethod
    def one_of_group_or_user(cls, data: Any) -> Any:
        user = data.get("user")
        group = data.get("group")
        if (user is not None) == (group is not None):
            raise ValueError(f"only one of [user, group] must be defined")
        return data


class MinioPolicyBinding(operator_core.NamespacedResource[MinioPolicyBindingSpec]):
    __oc_resource__ = {
        "api_version": "bfiola.dev/v1",
        "kind": "MinioPolicyBinding",
        "plural": "miniopolicybindings",
    }
    __oc_immutable_fields__ = {("group",), ("policy",), ("tenantRef",), ("user",)}


class TenantConfiguration(pydantic.BaseModel):
    name: str


class TenantSpec(pydantic.BaseModel):
    requestAutoCert: bool
    configuration: TenantConfiguration


class Tenant(operator_core.NamespacedResource[TenantSpec]):
    __oc_resource__ = {
        "api_version": "minio.min.io/v2",
        "kind": "Tenant",
        "plural": "tenants",
    }
