# Switchyard Helm chart

Switchyard, a cost-aware, OpenAI-compatible LLM gateway, on Kubernetes: the
high-availability tier of [DESIGN §18.1](https://github.com/itszarmik/Switchyard/blob/main/docs/DESIGN.md), with
three or more replicas, optional autoscaling, and the Redis and Postgres
you already run. The chart deploys only the gateway.

The full guide, from pulling the private image to troubleshooting, is
[docs/kubernetes.md](https://github.com/itszarmik/Switchyard/blob/main/docs/kubernetes.md).

```sh
kubectl create namespace switchyard
kubectl label namespace switchyard pod-security.kubernetes.io/enforce=restricted
helm install switchyard deploy/helm/switchyard -n switchyard -f secrets.yaml --wait
helm test switchyard -n switchyard
```

where `secrets.yaml` (never committed) holds at least:

```yaml
secrets:
  env:
    SWITCHYARD_ADMIN_TOKEN: "<openssl rand -base64 32>"
    DATABASE_URL: postgres://switchyard:<password>@postgres.internal:5432/switchyard?sslmode=require
    REDIS_CACHE_URL: rediss://:<password>@redis-cache.internal:6380/0
    REDIS_STATE_URL: rediss://:<password>@redis-state.internal:6380/0
    SWITCHYARD_API_KEY_HASH: sha256:<from switchyard keys generate>
    OPENAI_API_KEY: "<your key>"
```

or set `secrets.create=false` and `secrets.existingSecret` to a Secret with
the same keys (better for production: Helm keeps `secrets.env` in every
release revision).

Upgrade with the same values, and let Helm roll a failed upgrade back:
a bad config makes the new pods crash-loop, and the old pods keep serving
only until they restart, because they mount the same, updated ConfigMap
and Secret.

```sh
helm upgrade switchyard deploy/helm/switchyard -n switchyard -f values.yaml -f secrets.yaml \
  --wait --rollback-on-failure        # Helm 3: --atomic
```

docs/kubernetes.md ("Upgrades") shows how to validate the rendered config
with `switchyard config validate` first.

The chart's NetworkPolicy, on by default, lets only pods in the release's
namespace reach the admin port 9090 (`/metrics`, `/admin/v1/*`); list your
Prometheus in `networkPolicy.metricsFrom`. It needs a network plugin that
enforces NetworkPolicy: on one that does not, port 9090 is open to the
whole cluster.

## Layout

| Path | What it is |
|---|---|
| `values.yaml` | Every value, each with a comment. |
| `values.schema.json` | Types and ranges; unknown keys are rejected. |
| `files/switchyard.yaml` | The default config, which `config` is merged over (maps merge, lists replace). |
| `files/catalog/` | A copy of [configs/catalog](https://github.com/itszarmik/Switchyard/blob/main/configs/catalog), mounted at `/etc/switchyard/catalog`. |
| `files/alerts.yml` | A copy of [deploy/prometheus/alerts.yml](https://github.com/itszarmik/Switchyard/blob/main/deploy/prometheus/alerts.yml), for the PrometheusRule. |
| `templates/` | Deployment, Services, ConfigMaps, Secret, ServiceAccount, PodDisruptionBudget, HorizontalPodAutoscaler, NetworkPolicy, ServiceMonitor, PrometheusRule, and the `helm test` pod. |
| `ci/default-values.yaml` | The smallest valid install. |
| `ci/full-values.yaml` | Every option on, for lint and schema checks. |

The two copies under `files/` must stay identical to their sources:
`make helm-lint` fails when they drift. After changing the catalog or the
alerts, copy them again:

```sh
cp configs/catalog/*.yaml deploy/helm/switchyard/files/catalog/
cp deploy/prometheus/alerts.yml deploy/helm/switchyard/files/alerts.yml
```

`make helm-lint` lints the chart with each `ci/*-values.yaml` and runs the
render tests; `make helm-test` installs it on a throwaway kind cluster and
runs the end-to-end test (test/helm).
