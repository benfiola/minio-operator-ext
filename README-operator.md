# operator

Deploy minio-operator-ext, an operator that allows for declarative management of MinIO resources

**Homepage:** <https://github.com/benfiola/minio-operator-ext>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Specify which node's affinities operator workloads will prefer |
| fullnameOverride | string | `""` | Override the fully qualified name of resources created by this chart |
| image.pullPolicy | string | `"IfNotPresent"` | Define the pull policy for workloads using this image |
| image.repository | string | `"docker.io/benfiola/minio-operator-ext"` | Specify the repository to pull the image from |
| image.tag | string | `""` | Define the image tag to use If unset, uses the chart's app version. |
| imagePullSecrets | list | `[]` | Specify (if necessary) secrets required to pull the image.  More info: https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/ |
| livenessProbe | object | `{"failureThreshold":5,"httpGet":{"path":"/healthz","port":8888},"initialDelaySeconds":10,"periodSeconds":10,"successThreshold":1,"timeoutSeconds":5}` | Define the operator pod's liveness probe |
| nameOverride | string | `""` | Override the chart name |
| nodeSelector | object | `{}` | Specify which node operator workloads should be scheduled to |
| podAnnotations | object | `{}` | Sets annotations for all pods created by this chart. |
| podLabels | object | `{}` | Sets labels to set for all pods created by this chart. |
| podSecurityContext | object | `{}` | Sets the operator pod's security context |
| rbac.create | bool | `true` | Specifies whether a ClusterRole and ClusterRoleBinding should be created |
| rbac.name | string | `""` | The name of the ClusterRole and ClusterRoleBinding to create. If not set and create is true, a name is generated using the fullname template |
| readinessProbe | object | `{"failureThreshold":5,"httpGet":{"path":"/healthz","port":8888},"initialDelaySeconds":10,"periodSeconds":10,"successThreshold":1,"timeoutSeconds":5}` | Define the operator pod's readiness probe |
| resources | object | `{}` | Sets the operator pod's resource requests and limits |
| securityContext | object | `{"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsNonRoot":true,"runAsUser":65534}` | Sets the operator deployment's security context |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account |
| serviceAccount.automount | bool | `true` | Automatically mount a ServiceAccount's API credentials? |
| serviceAccount.create | bool | `true` | Specifies whether a service account should be created |
| serviceAccount.name | string | `""` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template |
| tolerations | list | `[]` | Specify which node's taints operator workloads will tolerate |
