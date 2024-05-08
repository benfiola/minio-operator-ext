# minio-operator-ext

The [MinIO Operator](https://github.com/minio/operator) currently is capable of deploying MinIO tenants - but does not expose any mechanisms by which one could declaratively manage resources within a MinIO tenant.

This repo extends the MinIO Operator and provides an additional [operator](./operator) and [CRDs](./manifests/crds.yaml) that allow one to declaratiely manage users, buckets, policies and policy bindings.

This is primarily a stopgap until support for minio resources lands in the official operator - but fulfills a personal need in the meantime. Feel free to fork or provide PRs to improve the operator!

## Deployment

To deploy the operator:

- Deploy the [CRDs](./manifests/crds.yaml)
- Deploy the operator ([example](./manifests/example_deployment.yaml))
- Deploy some custom resources ([example](./manifests/example_resources.yaml))

### Image

The operator is hosted on docker hub and can be found at [docker.io/benfiola/minio-operator-ext](https://hub.docker.com/r/benfiola/minio-operator-ext).

The following arguments/environment variables configure the operator:

| CLI             | Env                              | Default | Description                                                                      |
| --------------- | -------------------------------- | ------- | -------------------------------------------------------------------------------- |
| _--log-level_   | _MINIO_OPERATOR_EXT_LOG_LEVEL_   | `info`  | Logging verbosity for the operator                                               |
| _--kube-config_ | _MINIO_OPERATOR_EXT_KUBE_CONFIG_ | `null`  | Optional path to a kubeconfig file. When omitted, uses in-cluster configuration. |

### RBAC

The operator requires the a service account with the following RBAC settings:

- Get/Watch/List/Patch `minio.min.io/v2/Tenants` - the operator needs to discover and inspect minio tenants. Patch is required to allow `kopf` to add finalizers to the tenants so the framework can monitor resource deletion.
- Create `Events` - allows `kopf` to publish operator events
- Get `Secrets|Services` - the operator needs to inspect tenant secrets to obtain admin credentials and tenant services to determine the internal minio console endpoint
- Get/List/Patch/Watch `bfiola.dev/v1/MinioBuckets|MinioPolicies|MinioUsers|MinioPolicyBindings` - the operator needs to manage its own resources

## Limitations

Not all minio resource properties can be updated. These properties are treated as immutable. Attempts to modify immutable properties will be ignored and warning events will be logged to the resource in question.

Some examples of immutable properties:

- Bucket names
- User access keys
- Policy names
