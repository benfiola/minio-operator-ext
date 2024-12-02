# minio-operator-ext

The [MinIO Operator](https://github.com/minio/operator) currently is capable of deploying MinIO tenants - but does not expose any mechanisms by which one could declaratively manage resources within a MinIO tenant.

This repo extends the MinIO Operator (i.e., minio-operator-ext(ension)) - providing an additional [operator](./internal/operator/operator.go) and [CRDs](./charts/crds/templates/crds.yaml) that allow one to declaratiely manage users, buckets, policies and policy bindings.

## Resources

Currently, this operator manages the following resources:

- Users (`MinioUser`)
- Buckets (`MinioBucket`)
- Groups (`MinioGroup`)
- Group Membership (`MinioGroupBinding`)
- Policies (`MinioPolicy`)
- Policy Membership (`MinioPolicyBinding`)

Examples of these resources can be found [here](./manifests/example-resources.yaml).

## Installation

> [!IMPORTANT]
>
> Read [this](https://github.com/benfiola/minio-operator-ext/issues/16#issuecomment-2401553200) if you are updating from version 1.X.X to version 2.X.X of the operator.

Use [helm](https://helm.sh/) to install the operator:

```shell
# add the minio-operator-ext helm chart repository
helm repo add minio-operator-ext https://benfiola.github.io/minio-operator-ext/charts
# deploy the crds
helm install minio-operator-ext-crds minio-operator-ext/crds
# deploy the operator
helm install minio-operator-ext-operator minio-operator-ext/operator
```

Documentation for these helm charts can be found [here](https://benfiola.github.io/minio-operator-ext/).

### Image

The operator is hosted on docker hub and can be found at [docker.io/benfiola/minio-operator-ext](https://hub.docker.com/r/benfiola/minio-operator-ext).

The following arguments/environment variables configure the operator:

| CLI             | Env                              | Default | Description                                                                      |
| --------------- | -------------------------------- | ------- | -------------------------------------------------------------------------------- |
| _--log-level_   | _MINIO_OPERATOR_EXT_LOG_LEVEL_   | `info`  | Logging verbosity for the operator                                               |
| _--kube-config_ | _MINIO_OPERATOR_EXT_KUBE_CONFIG_ | `null`  | Optional path to a kubeconfig file. When omitted, uses in-cluster configuration. |

### RBAC

The operator requires the a service account with the following RBAC settings:

| Resource                         | Verbs                    | Why                                                                                                                 |
| -------------------------------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------- |
| minio.min.io/v2/Tenant           | Get                      | Used to discover MinIO tenants                                                                                      |
| v1/ConfigMap                     | Get                      | Used to obtain the CA bundle used to generate a MinIO tenant's TLS certificates (- for HTTP client cert validation) |
| v1/Secret                        | Get                      | Used to fetch a MinIO tenant's configuration (which is stored as a secret)                                          |
| v1/Services                      | Get                      | Used to determine a MinIO tenant's internal endpoint                                                                |
| bfiola.dev/v1/MinioBucket        | Get, Watch, List, Update | Required for the operator to manage minio bucket resources                                                          |
| bfiola.dev/v1/MinioGroup         | Get, Watch, List, Update | Required for the operator to manage minio group resources                                                           |
| bfiola.dev/v1/MinioGroupBinding  | Get, Watch, List, Update | Required for the operator to manage minio group membership                                                          |
| bfiola.dev/v1/MinioPolicy        | Get, Watch, List, Update | Required for the operator to manage minio policies                                                                  |
| bfiola.dev/v1/MinioPolicyBinding | Get, Watch, List, Update | Required for the operator to manage minio policy attachments                                                        |
| bfiola.dev/v1/MinioUser          | Get, Watch, List, Update | Required for the operator to manage minio user resources                                                            |

## Limitations

Some minio resource properties are ignored on update. This is done by design to prevent accidental, destructive changes.

| Property                 | Why                                                          |
| ------------------------ | ------------------------------------------------------------ |
| MinioBucket.Spec.Name    | Can break MinioPolicy resources, delete data                 |
| MinioGroup.Spec.Name     | Can break MinioGroupBinding and MinioPolicyBinding resources |
| MinioUser.Spec.AccessKey | Can break MinioGroupBinding and MinioPolicyBinding resources |
| MinioPolicy.Spec.Name    | Can break MinioPolicyBinding resources                       |

## Migrations

> [!CAUTION]
>
> The operator is designed to manage the entire lifecycle of its resources. This feature allows the operator to 'take ownership' of Minio objects it did not originally create.  Once the operator takes ownership of these existing Minio objects - it _can_ and _will_ modify them to match the state of its corresponding Kubernetes resources!

In more complex scenarios, users might want the operator to manage existing Minio objects. To allow existing objects to be managed by the operator, set the `.spec.migrate` field to _true_ for these resources.

```yaml
apiVersion: bfiola.dev/v1
kind: MinioBucket
...
spec:
  ...
   name: a
   migrate: true # <- add this
```

> [!NOTE]
>
> This spec field is _automatically_ removed by the operator once it takes ownership of a resource.

## Development

I personally use [vscode](https://code.visualstudio.com/) as an IDE. For a consistent development experience, this project is also configured to utilize [devcontainers](https://containers.dev/). If you're using both - and you have the [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers) installed - you can follow the [introductory docs](https://code.visualstudio.com/docs/devcontainers/tutorial) to quickly get started.

NOTE: Helper scripts are written under the assumption that they're being executed within a dev container.

### Installing tools

From the project root, run the following to install useful tools. Currently, this includes:

- helm
- helm-docs
- kubectl
- lb-hosts-manager
- mc
- minikube

```shell
cd /workspaces/minio-operator-ext
make install-tools
```

### Creating a development environment

From the project root, run the following to create a development environment to test the operator with:

```shell
cd /workspaces/minio-operator-ext
make dev-env
```

This will:

- Create a new minikube cluster
- Install the minio operator
- Create a minio tenant
- Deploy an ldap server
- Apply the [custom resources](./manifests/crds.yaml)
- Apply the [example resources](./manifests/example-resources.yaml)
- Wait for minio tenant to be accessible
- Forward minio tenant and ldap services to be available locally under their cluster-local DNS names

### Run end-to-end tests

With a development environment deployed, you can run end-to-end operator tests to confirm the operator functions as expected:

```shell
cd /workspaces/minio-operator-ext
make e2e-test
```

### Testing LDAP identities

After creating a local development cluster, you can configure minio to use the deployed LDAP server as its identity provider:

```shell
cd /workspaces/minio-operator-ext
make set-minio-identity-provider-ldap
```

NOTE: With an identity provider configured, attempts to operate on builtin identities will fail.

### Creating a debug script

Copy the [./dev/dev.go.template](./dev/dev.go.template) script to `./dev/dev.go`, then run it to start the operator. `./dev/dev.go` is ignored by git and can be modified as needed to help facilitate local development.

Additionally, the devcontainer is configured with a vscode launch configuration that points to `./dev/dev.go`. You should be able to launch (and attach a debugger to) the webhook via this vscode launch configuration.
