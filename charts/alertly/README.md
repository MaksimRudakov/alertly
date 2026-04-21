# alertly

![Version: 0.0.2](https://img.shields.io/badge/Version-0.0.2-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.3](https://img.shields.io/badge/AppVersion-0.0.3-informational?style=flat-square)

Webhook-to-Telegram bridge for Alertmanager and Kubewatch

**Homepage:** <https://github.com/MaksimRudakov/alertly>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Maksim Rudakov |  | <https://github.com/MaksimRudakov> |

## Source Code

* <https://github.com/MaksimRudakov/alertly>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Pod affinity rules. |
| config | object | see `values.yaml` | Alertly runtime configuration. Serialized verbatim into the ConfigMap mounted at `/etc/alertly/config.yaml`. |
| env | object | `{"LOG_LEVEL":"info"}` | Env vars passed to the container. Keys become env names, values are quoted as strings. |
| extraEnv | list | `[]` | Extra env vars in full k8s EnvVar form (supports `valueFrom`). |
| extraManifests | list | `[]` | Raw manifests rendered as-is. Use for PodMonitor/ServiceMonitor, PDB, NetworkPolicy, etc. without forking the chart.  Example — PodMonitor for kube-prometheus-stack: extraManifests:   - apiVersion: monitoring.coreos.com/v1     kind: PodMonitor     metadata:       name: alertly       labels:         release: kube-prometheus-stack     spec:       selector:         matchLabels:           app.kubernetes.io/name: alertly       podMetricsEndpoints:         - port: http           path: /metrics           interval: 30s  Example — PodDisruptionBudget: extraManifests:   - apiVersion: policy/v1     kind: PodDisruptionBudget     metadata:       name: alertly     spec:       maxUnavailable: 0       selector:         matchLabels:           app.kubernetes.io/name: alertly |
| fullnameOverride | string | `""` | Override full release name. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/maksimrudakov/alertly"` | Container image repository. |
| image.tag | string | `""` | Image tag. Overrides `Chart.AppVersion` when set. |
| imagePullSecrets | list | `[]` | Pull secrets for private registries. |
| ingress.annotations | object | `{}` | Ingress annotations (basic-auth, cert-manager, rate-limits, etc). |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Enable Ingress for the alertly webhook endpoint. |
| ingress.hosts | list | `[{"host":"alertly.example.com","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress host/path rules. |
| ingress.tls | list | `[]` | Ingress TLS blocks. |
| nameOverride | string | `""` | Override chart name. |
| nodeSelector | object | `{}` | Pod nodeSelector. |
| podAnnotations | object | `{}` | Extra annotations for the pod. |
| podLabels | object | `{}` | Extra labels for the pod. |
| podSecurityContext | object | `{"fsGroup":65532,"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context. |
| priorityClassName | string | `""` | Pod priorityClassName. |
| probes.liveness | object | `{"failureThreshold":3,"httpGet":{"path":"/healthz","port":"http"},"initialDelaySeconds":5,"periodSeconds":10,"timeoutSeconds":2}` | Liveness probe. Matches unconditional `/healthz`. |
| probes.readiness | object | `{"failureThreshold":3,"httpGet":{"path":"/readyz","port":"http"},"initialDelaySeconds":2,"periodSeconds":5,"timeoutSeconds":2}` | Readiness probe. `/readyz` flips unready on sustained 5xx from Telegram or during startup getMe. |
| reloader.enabled | bool | `false` | Add `reloader.stakater.com/auto: "true"` annotation to the Deployment to auto-restart on ConfigMap/Secret content changes. Requires stakater/Reloader in the cluster. |
| replicaCount | int | `1` | Number of alertly replicas. alertly is stateless; 1 is enough for most setups. |
| resources | object | `{"limits":{"cpu":"200m","memory":"128Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | CPU/memory requests and limits. |
| secret.create | bool | `true` | Create the Secret with tokens provided in `secret.values`. WARNING — NOT for production; tokens end up in Helm history. For production set `create: false` and supply the Secret via external-secrets/sealed-secrets/vault. |
| secret.existingSecret | string | `""` | Name of an existing Secret to use when `create: false`. Defaults to release fullname when empty. |
| secret.keys | object | `{"botToken":"TELEGRAM_BOT_TOKEN","webhookAuth":"WEBHOOK_AUTH_TOKEN"}` | Key names inside the Secret. Must match the env vars alertly reads (`TELEGRAM_BOT_TOKEN`, `WEBHOOK_AUTH_TOKEN`). |
| secret.values | object | `{"telegramBotToken":"","webhookAuthToken":""}` | Token values used only when `create: true`. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Container security context. Hardened defaults — read-only rootfs, non-root, all caps dropped. |
| service.annotations | object | `{}` | Extra annotations for the Service. |
| service.port | int | `8080` | Service port. |
| service.type | string | `"ClusterIP"` | Service type. Default ClusterIP — Alertmanager and Kubewatch typically run in the same cluster. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| serviceAccount.automountServiceAccountToken | bool | `false` | Whether to mount the ServiceAccount token into the pod. alertly does not talk to the Kubernetes API. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount for the pod. |
| serviceAccount.name | string | `""` | Use an existing ServiceAccount instead of creating one. Falls back to release fullname when empty and `create: true`. |
| terminationGracePeriodSeconds | int | `30` | Grace period before SIGKILL. |
| tolerations | list | `[]` | Pod tolerations. |
| topologySpreadConstraints | list | `[]` | Pod topology spread constraints. |

