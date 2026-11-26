# Kubernetes

The Helm chart in [deploy/helm/switchyard](../deploy/helm/switchyard) runs
the high-availability tier of DESIGN §18.1: three or more gateway replicas,
optional autoscaling, and the Redis and Postgres you already run. The chart
deploys only the gateway. Its state lives in two Redis instances (an
evictable cache, and the budgets and rate limits every replica shares) and
in the Postgres usage ledger, which you provide as URLs.

What the chart creates:

| Object | Name | Notes |
|---|---|---|
| Deployment | `<fullname>` | The gateway pods. Rolling updates add a pod before removing one. |
| Service | `<fullname>` | The API on port 8080. Never the admin listener. |
| ConfigMap | `<fullname>-config`, `<fullname>-catalog` | The rendered config and the model catalog, mounted as directories. |
| Secret | `<fullname>` | From `secrets.env`, unless you name an `existingSecret`. |
| ServiceAccount | `<fullname>` | No token mounted. |
| PodDisruptionBudget | `<fullname>` | `maxUnavailable: 1`. |
| HorizontalPodAutoscaler | `<fullname>` | With `autoscaling.enabled`. |
| Service | `<fullname>-metrics` | With `metrics.service.enabled` or a ServiceMonitor: the admin listener on 9090. |
| ServiceMonitor, PrometheusRule | `<fullname>` | With the Prometheus Operator. |
| NetworkPolicy | `<fullname>` | On by default: port 9090 only from the release's namespace. |
| Pod (test hook) | `<fullname>-test` | Runs on `helm test`. |

`<fullname>` is `<release>-switchyard`, or the release name alone when it
already contains "switchyard".

The pods pass Pod Security "restricted": they run as UID 65532 with a
read-only root filesystem, no capabilities, no privilege escalation, the
`RuntimeDefault` seccomp profile and no service-account token. Secrets reach
them only as environment variables from the Secret; the ConfigMaps and the
pod spec refer to them as `${VAR}`.

## Contents

- [Requirements](#requirements)
- [The image](#the-image)
- [Install](#install): [with secrets.env](#with-secretsenv) ·
  [with an existing Secret](#with-an-existing-secret) ·
  [checking it](#checking-the-install)
- [Redis and Postgres](#redis-and-postgres)
- [Values](#values)
- [Changing the config](#changing-the-config)
- [Upgrades](#upgrades)
- [Shutdown and sizing](#shutdown-and-sizing)
- [Autoscaling](#autoscaling)
- [Metrics and alerts](#metrics-and-alerts)
- [NetworkPolicy](#networkpolicy)
- [Troubleshooting](#troubleshooting)

## Requirements

- Kubernetes 1.30 or later, and Helm (the chart is tested with Helm 4).
- A Redis for the cache and a Redis for state (budgets, rate limits, cache
  namespace versions), or one Redis for both; see
  [Redis and Postgres](#redis-and-postgres).
- Postgres for the usage ledger (the compose setup runs 17). The gateway
  creates and migrates its tables at startup.
- Access to the gateway image (next section).
- At least one provider key (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
  `GEMINI_API_KEY` or `TOGETHER_API_KEY`).

The chart is in this repository at `deploy/helm/switchyard`, and every
GitHub release from v0.2.0 attaches it as `switchyard-<version>.tgz`. The
commands below use the directory in a checkout; the `.tgz` (a path or its
release URL) works in its place.

## The image

The chart's default image, `ghcr.io/itszarmik/switchyard:v<appVersion>`, is
**private**. Either give the cluster a pull secret or use an image you
build.

**A pull secret.** With a GitHub token that has `read:packages` and access
to the package, kept in a file only you can read (`ghcr-token`), so it
never appears on a command line, in your shell history or in the process
list:

```sh
kubectl create namespace switchyard
umask 077
printf '{"auths":{"ghcr.io":{"auth":"%s"}}}' \
  "$(printf '%s:%s' <github-user> "$(cat ghcr-token)" | base64 | tr -d '\n')" > ghcr-pull.json
kubectl -n switchyard create secret generic ghcr-pull \
  --type=kubernetes.io/dockerconfigjson --from-file=.dockerconfigjson=ghcr-pull.json
rm ghcr-pull.json
```

and install with `--set 'imagePullSecrets[0].name=ghcr-pull'`. The helm test
pod uses the same secrets.

**Your own image.** Build `deploy/Dockerfile` and push it to a registry the
cluster can pull from. For both common node architectures in one image:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -f deploy/Dockerfile -t registry.example.com/platform/switchyard:v0.4.0 --push .
```

then set `image.repository` (and `image.tag` if it is not `v<appVersion>`).
Set `image.digest` to the pushed digest to pin it. `make docker` builds a
single-architecture `switchyard:dev` for local clusters: `kind load
docker-image switchyard:dev` and `--set image.repository=switchyard
--set image.tag=dev --set image.pullPolicy=Never`.

## Install

Use a namespace that enforces Pod Security "restricted", so nothing less
hardened can run beside the gateway:

```sh
kubectl create namespace switchyard   # unless you created it above
kubectl label namespace switchyard pod-security.kubernetes.io/enforce=restricted
```

You need an admin token and a first gateway API key:

```sh
openssl rand -base64 32               # the admin token (at least 32 characters)
docker run --rm ghcr.io/itszarmik/switchyard:v0.4.0 keys generate
                                      # or: switchyard keys generate
```

`keys generate` prints the key once (`sy_...`, give it to the client) and
its hash (`sha256:...`); only the hash goes into the cluster.

### With secrets.env

The chart creates the Secret from `secrets.env`. Put the secrets in a file
of their own that is never committed, for example `secrets.yaml`:

```yaml
secrets:
  env:
    SWITCHYARD_ADMIN_TOKEN: "<the admin token>"
    DATABASE_URL: postgres://switchyard:<password>@postgres.internal:5432/switchyard?sslmode=require
    REDIS_CACHE_URL: rediss://:<password>@redis-cache.internal:6380/0
    REDIS_STATE_URL: rediss://:<password>@redis-state.internal:6380/0
    SWITCHYARD_API_KEY_HASH: sha256:<the key's hash>
    OPENAI_API_KEY: "<your key>"
```

```sh
helm install switchyard deploy/helm/switchyard -n switchyard \
  -f secrets.yaml --set 'imagePullSecrets[0].name=ghcr-pull' --wait
```

Rendering stops with a message naming the variable, never its value, when
one of the four required ones is missing, when the admin token is shorter
than 32 characters or contains whitespace (often a trailing newline, from
`--set-file` or a YAML `|` block), or when the config refers to a `${VAR}`
without a default that `secrets.env` and `extraEnv` do not set
(`SWITCHYARD_API_KEY_HASH`, with the default tenant). A change to
`secrets.env` rolls the pods on the next `helm upgrade`.

Keep secrets off command lines, where shell history and the process list
keep them: pass them in a file (`-f secrets.yaml`, or `--set-file
secrets.env.OPENAI_API_KEY=openai-key` from a file you made with `umask
077`), never `--set secrets.env.X=...`.

Helm keeps every release revision's values, `secrets.env` included, in a
Secret of its own in the namespace (`sh.helm.release.v1.<release>.v<n>`,
the last 10 revisions by default, `--history-max`), and `helm get values
--revision <n>` prints them. So after rotating a leaked key through
`secrets.env`, the old value stays readable there until those revisions
age out. Anyone who can read Secrets in the namespace can read the chart's
Secret anyway, but for production prefer an existing Secret (next
section), which keeps the values out of the release history, and lets
them come from a vault.

### With an existing Secret

Create the Secret yourself (or with sealed-secrets or External Secrets).
Every key becomes an environment variable. From a file only you can read,
so no value is on a command line:

```sh
umask 077
cat > switchyard.env <<'EOF'
SWITCHYARD_ADMIN_TOKEN=<openssl rand -base64 32>
DATABASE_URL=postgres://...
REDIS_CACHE_URL=rediss://...
REDIS_STATE_URL=rediss://...
SWITCHYARD_API_KEY_HASH=sha256:...
OPENAI_API_KEY=...
EOF
kubectl -n switchyard create secret generic switchyard-secrets --from-env-file=switchyard.env

helm install switchyard deploy/helm/switchyard -n switchyard \
  --set secrets.create=false --set secrets.existingSecret=switchyard-secrets \
  --set 'imagePullSecrets[0].name=ghcr-pull' --wait
```

The chart cannot see this Secret's keys, so a missing variable shows up as a
pod that exits at startup naming it (see [Troubleshooting](#troubleshooting)).
Nor can it see changes to it: after editing the Secret, restart the pods
with `kubectl -n switchyard rollout restart deployment/switchyard`. More
Secrets can be added with `extraEnvFrom`, for example provider keys owned
by another team.

### Checking the install

```sh
kubectl -n switchyard rollout status deployment/switchyard
helm test switchyard -n switchyard
kubectl -n switchyard port-forward service/switchyard 8080:8080 &
curl -s localhost:8080/v1/chat/completions -H "Authorization: Bearer sy_..." \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"Say hello"}]}'
```

`helm test` runs a pod with the gateway image that GETs `/readyz` through
the Service. Right after `helm install --wait` the Service may not be
programmed yet, so the pod retries until it passes or `helm test`'s
`--timeout` (5 minutes by default) ends it. With `server.tls` on, the public listener serves HTTPS with a
certificate that does not name the Service's internal DNS name, and the
test fails; probe the admin listener by hand instead (`kubectl
port-forward deployment/switchyard 9090:9090` and `curl
localhost:9090/readyz`).

## Redis and Postgres

**Redis.** Two instances are best, as in the compose setup:

| | Cache (`REDIS_CACHE_URL`) | State (`REDIS_STATE_URL`) |
|---|---|---|
| Holds | Exact and semantic cache entries | Budgets, rate limits, stream slots, cache namespace versions |
| Eviction | `allkeys-lru`, with a `maxmemory` | `noeviction` |
| Persistence | None needed | AOF or your provider's persistence |
| When it is down | Every lookup is a miss; requests are served | `budgets.fail_mode` and `rate_limits.fail_mode` decide (open by default) |

One instance can serve both only if it does not evict: give the two URLs
different database numbers (`/0`, `/1`). An evicting state Redis would drop
budget counters and rate-limit state. URLs are `redis://` or, with TLS,
`rediss://`; a password goes in the URL (`rediss://:<password>@host:port/0`,
or `rediss://<user>:<password>@...` with ACLs). Managed services such as
ElastiCache, Memorystore and Azure Cache work; Redis Cluster mode is not
supported.

**Postgres.** `DATABASE_URL` is a libpq URL or DSN. With a managed
database, use `sslmode=require` or, with the provider's CA bundle mounted
from a Secret (`extraVolumes`, `extraVolumeMounts`), `sslmode=verify-full`
and `sslrootcert=/etc/switchyard/tls/ca.pem`. The role needs to create
tables in its database the first time: the ledger migrates at startup,
under an advisory lock, so replicas starting together are safe. Postgres
must be reachable when a pod starts (the pod exits after 30 s otherwise and
is restarted); while it is down later, usage events queue in memory and the
gateway keeps serving ([runbook](runbook.md#postgres-down)).

Every replica opens its own connections to both Redis instances and to
Postgres. With autoscaling, size their connection limits for
`autoscaling.maxReplicas`. If you use a NetworkPolicy with `egress`, allow
these hosts and ports ([NetworkPolicy](#networkpolicy)).

## Values

`values.yaml` describes every value. The ones most installs touch:

| Value | Default | What it does |
|---|---|---|
| `replicaCount` | `3` | Replicas, unless `autoscaling.enabled`. |
| `image.repository`, `.tag`, `.digest` | `ghcr.io/itszarmik/switchyard`, `v<appVersion>`, none | The image. |
| `imagePullSecrets` | `[]` | Pull secrets for a private registry. |
| `secrets.create`, `.existingSecret`, `.env` | `true`, `""`, `{}` | Where the secret environment variables come from. |
| `extraEnv`, `extraEnvFrom` | `[]` | Plain variables (log level, base URLs) and more Secrets or ConfigMaps. |
| `config` | `{}` | The Switchyard config, merged over the chart's default ([next section](#changing-the-config)). |
| `catalog.bundled`, `.extraFiles` | `true`, `{}` | The bundled model catalog, and files to add or replace. |
| `service.type`, `.port`, `.nodePort`, `.externalTrafficPolicy` | `ClusterIP`, `8080`, none, none | The API Service. |
| `metrics.*` | off | The metrics Service, ServiceMonitor and PrometheusRule. |
| `resources` | 250m CPU, 256Mi requested; 512Mi limit | Per pod ([sizing](#shutdown-and-sizing)). |
| `probes.*` | | Probe timings. |
| `autoscaling.*` | off, 3 to 10 at 70 % CPU | The HorizontalPodAutoscaler. |
| `podDisruptionBudget.*` | on, `maxUnavailable: 1` | |
| `topologySpreadConstraints` | one per node where possible | Scheduling; also `affinity`, `nodeSelector`, `tolerations`, `priorityClassName`. |
| `terminationGracePeriodSeconds` | derived | See [Shutdown and sizing](#shutdown-and-sizing). |
| `extraVolumes`, `extraVolumeMounts` | `[]` | TLS certificates, CA bundles, `${file:...}` secrets. |
| `networkPolicy.*` | on: 9090 from the release's namespace only | See [NetworkPolicy](#networkpolicy). |

[ci/default-values.yaml](../deploy/helm/switchyard/ci/default-values.yaml)
is the smallest valid set; [ci/full-values.yaml](../deploy/helm/switchyard/ci/full-values.yaml)
turns every option on. `values.schema.json` rejects unknown keys and wrong
types before anything renders.

## Changing the config

The gateway reads `/etc/switchyard/config/switchyard.yaml`, rendered from
the chart's [files/switchyard.yaml](../deploy/helm/switchyard/files/switchyard.yaml)
(the compose config, adapted: Redis mode, the Postgres ledger, the four
providers, a default tenant with a monthly budget, `shutdown_delay: 5s`) with
your `config` value merged over it. Every key is in
[configuration.md](configuration.md).

The merge:

- **Maps merge** key by key, at every depth.
- **Lists replace.** A list you give (`providers`, `policies`, `tenants`,
  `models`, `include`, `allow`) replaces the default list whole; it is
  never appended to or merged by position. Copy the default entries you
  want to keep.
- **Anything else replaces**, `false`, `0` and `""` included.
- **`null` removes** a default key, so the gateway's own default applies.

```yaml
# values-prod.yaml
config:
  logging:
    level: warn                 # merged: format stays json
  server:
    shutdown_grace: 5m          # long streams; the grace period follows
  rate_limits:
    fail_mode: closed
  tenants:                      # replaces the default tenant list
    - id: default
      policy: default
      rate_limit: { requests_per_minute: 1200, max_concurrent_streams: 50 }
      keys:
        - id: default
          hash: ${SWITCHYARD_API_KEY_HASH}
        - id: batch-jobs
          hash: ${BATCH_KEY_HASH}   # add BATCH_KEY_HASH to the Secret
```

Write secrets only as `${VAR}` references: the rendered config is a
ConfigMap, readable by anyone who can read the namespace's ConfigMaps.
Rendering stops, naming the field, when one of the gateway's secret fields
(`server.admin_token`, `providers[].api_key`, `storage.redis.cache_url` and
`state_url`, `storage.ledger.dsn`) is anything but one reference
(`${VAR}`, `${VAR:-default}` or `${file:PATH}`), or when
`telemetry.tracing.endpoint` has a password in it. Key hashes are not
secret, but keeping them in the Secret keeps the config reusable. A literal
`$` is written `$$` (so `$${VAR}` is the text `${VAR}`, not a reference).

Keep `server.listen` on port 8080 and `server.admin_listen` at `:9090`: the
container ports, Service, probes and test use them, and rendering stops if
they change. With `catalog.bundled: false`, give your own catalog files in
`catalog.extraFiles` or override `include`.

To see what the gateway will get:

```sh
helm template switchyard deploy/helm/switchyard -f values-prod.yaml -f secrets.yaml \
  --show-only templates/configmap.yaml
```

[Upgrades](#upgrades) shows how to validate it with the binary before an
upgrade.

Base URLs and the `auto` allow list come from plain variables in the
default config, so a few common changes need no list override:

```yaml
extraEnv:
  - name: SWITCHYARD_ALLOW              # only route "auto" to OpenAI models
    value: "*@openai"
  - name: SWITCHYARD_OPENAI_BASE_URL    # an OpenAI-compatible proxy
    value: https://llm-proxy.internal/v1
  - name: SWITCHYARD_LOG_LEVEL
    value: debug
```

## Upgrades

```sh
helm upgrade switchyard deploy/helm/switchyard -n switchyard \
  -f values-prod.yaml -f secrets.yaml --wait --timeout 5m --rollback-on-failure
```

Pass the same values files every time: an upgrade renders from the chart's
defaults and what you give it, not from what is running.
`--rollback-on-failure` (Helm 4; `--atomic` in Helm 3) rolls a failed
upgrade back at once (see "A bad config" below).

**A config change rolls the pods.** The pod template carries a checksum of
the rendered config, of the catalog and of `secrets.env` (a hash; the
values are never copied), so any change replaces every pod, and
restart-only settings (listeners, storage, logging, telemetry, cache) take
effect too. The rollout adds one new pod (`maxSurge: 25%`), waits until it
is ready, then stops an old one (`maxUnavailable: 0`), so capacity never
drops. Each old pod stops gracefully: it fails readiness, keeps serving for
`shutdown_delay` while the Service stops sending it connections, then
finishes its in-flight requests, streams included.

The checksum of `secrets.env` is a plain SHA-256 in the pod template, so
anyone who can read pods can check a guess of the whole set of values
against it (and see when they change). That is harmless with
high-entropy secrets such as `openssl rand -base64 32` tokens and provider
keys; do not put guessable values in `secrets.env`.

The gateway can also hot-reload the mounted config (`POST /admin/v1/reload`
on a pod), but the chart's rolling restart makes that unnecessary, and a
reload would apply only to the pod you call.

**A bad config.** The gateway validates its config at startup and exits,
with every problem in its log, when it is invalid or a required variable is
missing. In a failed upgrade:

1. The first new pod crash-loops and never becomes ready, so the rollout
   stops there (`maxUnavailable: 0`).
2. The old pods keep serving, **but only until one restarts**: the upgrade
   has already replaced the config ConfigMap and the Secret, which every
   pod mounts. An old pod that restarts for any reason (a node drain, an
   eviction, an OOM kill, an HPA scale-up) reads the bad config too and
   crash-loops.
3. `helm upgrade --wait` fails after its timeout. With
   `--rollback-on-failure` (`--atomic` in Helm 3), Helm then rolls the
   release back: the previous config and Secret return and the new pod is
   removed, so nothing is left to break. Without it, roll back yourself at
   once: `helm rollback switchyard -n switchyard`.

To catch a bad config before the upgrade, render it and check it with
`switchyard config validate` (the same checks the gateway runs at startup;
the binary from a [release](https://github.com/itszarmik/Switchyard/releases)
or `make build`). This needs helm and kubectl, but no cluster; the
variables are placeholders, because only the config is checked:

```sh
dir=$(mktemp -d) && mkdir "$dir/config" "$dir/catalog"
helm template switchyard deploy/helm/switchyard -n switchyard -f values-prod.yaml -f secrets.yaml \
  --show-only templates/configmap.yaml > "$dir/config.yaml"
helm template switchyard deploy/helm/switchyard -n switchyard -f values-prod.yaml -f secrets.yaml \
  --show-only templates/configmap-catalog.yaml > "$dir/catalog.yaml"
kubectl label --local -f "$dir/config.yaml" x=y -o 'go-template={{index .data "switchyard.yaml"}}' \
  > "$dir/config/switchyard.yaml"
for f in $(kubectl label --local -f "$dir/catalog.yaml" x=y -o 'go-template={{range $k, $v := .data}}{{$k}} {{end}}'); do
  kubectl label --local -f "$dir/catalog.yaml" x=y -o "go-template={{index .data \"$f\"}}" > "$dir/catalog/$f"
done
SWITCHYARD_ADMIN_TOKEN=placeholder-admin-token-for-config-validate \
SWITCHYARD_API_KEY_HASH=sha256:0000000000000000000000000000000000000000000000000000000000000000 \
DATABASE_URL=postgres://placeholder/switchyard \
REDIS_CACHE_URL=redis://placeholder:6379/0 REDIS_STATE_URL=redis://placeholder:6379/1 \
  switchyard config validate -c "$dir/config/switchyard.yaml"
```

It prints `OK` (and one expected warning, that the admin listener is
reachable from other hosts), or every error with its line and a non-zero
exit status, so it also works as a CI step. Set any other variable your
config references without a default the same way, and remove `$dir`
afterwards.

**A new gateway version.** Upgrading the chart moves `image.tag` to its
`appVersion` unless you set the tag. The first new pod migrates the ledger
(under an advisory lock), and the rest follow. Read the
[CHANGELOG](../CHANGELOG.md) before going back to an older version.

## Shutdown and sizing

After SIGTERM the gateway (DESIGN §14.8):

1. fails `/readyz` and keeps serving for `server.shutdown_delay` (5 s in the
   chart's config; the gateway's own default is 0), so every kube-proxy and
   load balancer stops sending it new connections first;
2. stops accepting connections and lets in-flight requests finish for up to
   `server.shutdown_grace` (60 s);
3. within 15 s more, writes pending cache and shadow work, flushes the usage
   queue to Postgres, and drains spans and logs.

So it exits within `shutdown_delay + shutdown_grace + 15s`, and the chart
sets `terminationGracePeriodSeconds` to that: 80 s by default. It reads both
durations from the rendered config, applying the gateway's defaults (an
unset delay is 0 s; an unset or zero grace is 60 s), and understands whole
hours, minutes and seconds (`0s`, `90s`, `2m`, `1m30s`, `1h`); for anything
else (fractions, `ms`, `${VAR}` references) rendering stops and asks you to
set `terminationGracePeriodSeconds` yourself. Durations always need a unit:
the gateway refuses a bare `0` or `60`, and so does the chart. A pod killed before it finishes
loses its queued usage events. No `preStop` hook is needed:
`shutdown_delay` does its job.

- **Long streams.** Raise `config.server.shutdown_grace` to the longest
  stream you want to survive a rollout or a scale-down; the grace period
  follows. Streams still running after it are cut.
- **Slow load balancers.** If an external load balancer takes longer than
  5 s to stop routing to a terminating pod, raise `shutdown_delay`.
- **Resources.** In the one-hour soak at 1,000 requests a second in Redis
  mode, one gateway stayed under 245 MiB RSS
  ([test/load/RESULTS.md](../test/load/RESULTS.md)), so the defaults (256Mi
  requested, a 512Mi limit) leave room. The memory request should cover the
  steady state; the limit absorbs large request bodies (up to
  `server.max_request_bytes`, 10 MiB by default, each). CPU has no limit by
  default, to avoid throttling the latency of every request; the request is
  what the scheduler and autoscaler count. Go sizes its threads from the
  container's CPU limit when there is one.
- **Disruptions.** The PodDisruptionBudget lets a node drain evict one pod
  at a time; each takes up to the grace period to leave.
- **Placement.** By default pods spread over nodes (`ScheduleAnyway`, so a
  small cluster still schedules them). For zones, add a constraint on
  `topology.kubernetes.io/zone`; an entry without a `labelSelector` gets
  the chart's.

## Autoscaling

```yaml
autoscaling:
  enabled: true
  minReplicas: 3
  maxReplicas: 12
  targetCPUUtilizationPercentage: 70
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 600
      policies: [{ type: Pods, value: 1, periodSeconds: 120 }]
```

The HPA needs the metrics server. CPU utilisation is measured against
`resources.requests.cpu`. With autoscaling on, the chart renders the
Deployment's `replicas` as the running Deployment's current count (looked
up at install or upgrade; `minReplicas` when there is none yet), so `helm
upgrade` keeps whatever the HPA chose, and turning autoscaling on keeps
every pod running (a Deployment without the field would drop to 1).

The gateway's cost is mostly waiting on providers, so CPU can stay low
while connections pile up. DESIGN §18.1 also scales on in-flight requests:
with a Prometheus adapter exposing `switchyard_http_requests_in_flight` as
a pods metric, add it in `autoscaling.extraMetrics`:

```yaml
  extraMetrics:
    - type: Pods
      pods:
        metric: { name: switchyard_http_requests_in_flight }
        target: { type: AverageValue, averageValue: "200" }
```

Scale down slowly: each removed pod drains for up to `shutdown_grace`. Rate
limits, stream caps and budgets live in the state Redis, so they hold
whatever the replica count; the per-address authentication-failure limit is
per replica (see the [runbook](runbook.md#clients-rate-limited)).

## Metrics and alerts

`/metrics` is on the admin listener, port 9090 of every pod, and needs no
token. No Service exposes it unless asked, but the port itself is
reachable from any pod in the cluster unless a NetworkPolicy stops it: the
chart's, on by default, lets only pods in the release's namespace and
`networkPolicy.metricsFrom` in ([NetworkPolicy](#networkpolicy)). To
scrape it:

```yaml
metrics:
  service: { enabled: true }          # <fullname>-metrics, port 9090
  serviceMonitor:
    enabled: true                     # Prometheus Operator
    labels: { release: kube-prometheus-stack }
  prometheusRule:
    enabled: true
    labels: { release: kube-prometheus-stack }
```

The ServiceMonitor sets `job="switchyard"` on every target, which the
alerts expect (`SwitchyardDown` is `up{job="switchyard"} == 0`). The
PrometheusRule holds the rules of
[deploy/prometheus/alerts.yml](../deploy/prometheus/alerts.yml), each with a
`runbook_url` into [runbook.md](runbook.md). Import the Grafana dashboard
from [deploy/grafana/dashboards/switchyard.json](../deploy/grafana/dashboards/switchyard.json).
Without the Operator, scrape the metrics Service's endpoints with your own
job named `switchyard`, and load `alerts.yml` as a rule file.

The metrics port also serves `/admin/v1/*` (behind the admin token): keep
the metrics Service internal and limit the port to your Prometheus with
`networkPolicy.metricsFrom`. A Prometheus in another namespace must be
listed there, or the policy refuses its scrapes and `SwitchyardDown` fires;
`helm install` warns about this when the ServiceMonitor is on and
`metricsFrom` is empty. Tracing goes to an OTLP/HTTP collector:

```yaml
config:
  telemetry:
    tracing:
      enabled: true
      endpoint: http://otel-collector.observability.svc:4318
      sample_ratio: 0.05
```

## NetworkPolicy

The chart creates a NetworkPolicy by default (`networkPolicy.enabled:
true`): the API port 8080 from anywhere, the admin port 9090 only from
pods in the release's namespace, and egress unrestricted. To narrow it:

```yaml
networkPolicy:
  enabled: true
  ingressFrom:                  # who may call the API (port 8080); empty: anyone
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: ingress-nginx }
  metricsFrom:                  # who may reach port 9090; empty: this namespace
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: monitoring }
```

The policy covers the gateway pods only. Ingress is allowed on 8080 from
`ingressFrom` (and from the chart's test pod) and on 9090 from
`metricsFrom`; everything else is refused. The kubelet's probes are not
affected.

The cluster's network plugin must enforce NetworkPolicy: Calico, Cilium,
kind's default kindnet and most managed clusters' policy options do, but
some plugins (flannel on its own, for example) do not. On those the policy
is accepted and **does nothing**: port 9090 (`/metrics` without
authentication, `/admin/v1/*` behind the admin token) stays reachable from
every pod in the cluster. Check yours, for example by probing a pod's
port 9090 from another namespace. With `networkPolicy.enabled: false`,
`helm install` prints the same warning.

Egress is unrestricted by default, because the providers are on the
internet at addresses that change. Setting `egress` restricts it to DNS
(port 53, anywhere) plus your rules, which must cover Redis, Postgres,
every provider and the tracing collector:

```yaml
  egress:
    - to: [{ ipBlock: { cidr: 10.20.0.0/16 } }]          # Redis and Postgres
      ports: [{ port: 6380, protocol: TCP }, { port: 5432, protocol: TCP }]
    - to:                                                 # providers, over HTTPS
        - ipBlock:
            cidr: 0.0.0.0/0
            except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.0.0/16]
      ports: [{ port: 443, protocol: TCP }]
```

## Troubleshooting

Start with the pods' state and the gateway's log (JSON, one line per
request; the startup lines say which config and catalog files were loaded):

```sh
kubectl -n switchyard get pods -l app.kubernetes.io/component=gateway
kubectl -n switchyard describe pod <pod>
kubectl -n switchyard logs <pod> --previous | jq -c 'select(.msg != "request")'
```

| Symptom | Likely cause and fix |
|---|---|
| `helm install` stops with "secrets.env is missing ..." or "the config refers to X without a default" | Add the variable to `secrets.env`, or use an existing Secret that has it. |
| `helm install` stops with "values don't meet the specifications of the schema" | The message names the value (`at '/replicaCount'`) and what it expected. |
| "cannot derive terminationGracePeriodSeconds" | A shutdown duration is not plain h/m/s: set `terminationGracePeriodSeconds`. |
| `ImagePullBackOff` | The image is private: see [The image](#the-image). Check the pull Secret is in the release's namespace. |
| `CreateContainerConfigError`, "secret not found" | `secrets.existingSecret` names a Secret that does not exist in the namespace. |
| `CrashLoopBackOff`, log says a variable is not set | The config refers to `${VAR}` without a default and the Secret lacks it. With an existing Secret the chart cannot check this. |
| `CrashLoopBackOff`, log says the ledger cannot start | Postgres is unreachable, refuses the credentials, or TLS settings do not match. Test from a pod in the namespace; check any `egress` NetworkPolicy. |
| `helm install` stops with "config.X is a literal value" | A secret field of `config` holds a literal: write it as `${VAR}` and put the value in the Secret ([Changing the config](#changing-the-config)). |
| `CrashLoopBackOff`, config errors in the log | An invalid config: the log lists every problem with its file and line. Old pods keep serving only until they restart, so roll back now (`helm rollback`) or fix and upgrade; next time use `--rollback-on-failure` ([Upgrades](#upgrades)). |
| Pods `Running` but not ready | `/readyz` fails: `kubectl port-forward <pod> 9090:9090` and `curl localhost:9090/readyz` names the failing check (with `budgets.fail_mode: closed`, a down state Redis). |
| "violates PodSecurity" events | Something added to the pod (an `extraVolumes` hostPath, a sidecar through another tool) breaks "restricted"; the chart's own pods comply. |
| `helm test` fails or times out | The test pod retries until `--timeout`. No pod is ready, or a NetworkPolicy blocks the test pod (the chart's policy allows it), or the public listener serves TLS (see [Checking the install](#checking-the-install)). `kubectl logs <fullname>-test --previous` has the last failure. |
| Prometheus targets down, `SwitchyardDown` firing | The NetworkPolicy refuses a Prometheus outside the release's namespace: add it to `networkPolicy.metricsFrom`. |
| Every client gets 429 behind a load balancer | The auth-failure limit counts failures per client address; behind a proxy that hides addresses, all clients share one. Preserve client addresses (`service.externalTrafficPolicy: Local` with a LoadBalancer or NodePort Service) or set `config.rate_limits.auth_failures_per_minute: -1` and limit at the proxy ([runbook](runbook.md#clients-rate-limited)). |
| A Secret change has no effect | With `existingSecret`, run `kubectl rollout restart deployment/<fullname>`; environment variables are read at startup. |

The admin API works through a port-forward to any pod, with the admin token:

```sh
kubectl -n switchyard port-forward deployment/switchyard 9090:9090 &
curl -s -H "Authorization: Bearer $SWITCHYARD_ADMIN_TOKEN" localhost:9090/admin/v1/providers | jq .
```

A port-forward reaches one pod; breaker and latency state is per pod. For
everything else (provider outages, budgets, leaked keys), see the
[runbook](runbook.md): its compose commands have direct equivalents
(`kubectl logs`, `kubectl rollout restart`, the admin API above).
