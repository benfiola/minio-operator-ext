# minio-operator-ext

The [MinIO Operator](https://github.com/minio/operator) currently is capable of deploying MinIO tenants - but does not expose any mechanisms by which one could declaratively manage resources within a MinIO tenant.

This repo extends the MinIO Operator (i.e., minio-operator-ext(ension)) - providing an additional [operator](./minio_operator_ext/operator.py) and [CRDs](./manifests/crds.yaml) that allow one to declaratiely manage users, buckets, policies and policy bindings.

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

Installation is a two-step process:

- Deploy the [CRDs](./manifests/crds.yaml)
- Deploy the operator ([example](./manifests/example-deployment.yaml))

### Image

The operator is hosted on docker hub and can be found at [docker.io/benfiola/minio-operator-ext](https://hub.docker.com/r/benfiola/minio-operator-ext).

The following arguments/environment variables configure the operator:

| CLI             | Env                              | Default | Description                                                                      |
| --------------- | -------------------------------- | ------- | -------------------------------------------------------------------------------- |
| _--log-level_   | _MINIO_OPERATOR_EXT_LOG_LEVEL_   | `info`  | Logging verbosity for the operator                                               |
| _--kube-config_ | _MINIO_OPERATOR_EXT_KUBE_CONFIG_ | `null`  | Optional path to a kubeconfig file. When omitted, uses in-cluster configuration. |

### RBAC

The operator requires the a service account with the following RBAC settings:

| Resource               | Verbs                   | Why                                                                                                                 |
| ---------------------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------- |
| minio.min.io/v2/Tenant | Get, Watch, List, Patch | Used to discover MinIO tenants                                                                                      |
| v1/Event               | Create                  | Used to publish events whenever activity is performed                                                               |
| v1/ConfigMap           | Get                     | Used to obtain the CA bundle used to generate a MinIO tenant's TLS certificates (- for HTTP client cert validation) |
| v1/Secret              | Get                     | Used to fetch a MinIO tenant's configuration (which is stored as a secret)                                          |
| v1/Services            | Get                     | Used to determine a MinIO tenant's internal endpoint                                                                |

## Limitations

Not all minio resource properties can be updated. These properties are treated as immutable. Attempts to modify immutable properties will be ignored and warning events will be logged to the resource in question.

Some examples of immutable properties:

- Bucket names
- Group names
- User access keys
- Policy names

## Development

I personally use [vscode](https://code.visualstudio.com/) as an IDE. For a consistent development experience, this project is also configured to utilize [devcontainers](https://containers.dev/). If you're using both - and you have the [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers) installed - you can follow the [introductory docs](https://code.visualstudio.com/docs/devcontainers/tutorial) to quickly get started.

NOTE: Helper scripts are written under the assumption that they're being executed within a dev container.

### Creating a development environment

From the project root, run the following to create a development cluster to test the operator with:

```shell
cd /workspaces/minio-operator-ext
./dev/create-cluster.sh
```

This will:

- Delete an existing dev cluster if one exists
- Create a new dev cluster
- Install the minio operator
- Create a minio tenant
- Deploy an ldap server
- Apply the [custom resources](./manifests/crds.yaml)
- Apply the [example resources](./manifests/example-resources.yaml)
- Waits for minio tenant to finish deploying
- Forward all services locally under their cluster-local DNS names

### Testing LDAP identities

After creating a local development cluster, you can configure minio to use the deployed LDAP server as its identity provider:

```shell
cd /workspaces/minio-operator-ext
./dev/use-ldap.sh
```

NOTE: With an identity provider configured, attempts to operate on builtin identities will fail.

### Creating a launch script

Copy the [dev.template.py](./dev.template.py) script to `dev.py`, then run it to start the operator against the local development environment.

If placed in the top-level directory, `dev.py` is gitignored and you can change this file as needed without worrying about committing it to git.

Additionally, the devcontainer is configured with vscode launch configurations that point to a top-level `dev.py` file. You should be able to launch (and attach a debugger to) the operator by launching it natively through vscode.
