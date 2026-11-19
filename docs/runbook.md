# Runbook

What to do when something goes wrong with a Switchyard deployment. Each
section gives the **symptom**, the **signal** (the metric, alert or log
line that shows it), the **impact** on clients, and **what to do**. The
alert rules are in [deploy/prometheus/alerts.yml](../deploy/prometheus/alerts.yml)
and link here; the panels named below are on the Switchyard Grafana
dashboard ([deploy/grafana/dashboards/switchyard.json](../deploy/grafana/dashboards/switchyard.json)).

The design rule behind every degraded mode (DESIGN §14.7): **optimizations
fail open** (caches, tracing) and **money and security fail closed** (auth,
hard budgets with `budgets.fail_mode: closed`, per-request cost caps). Rate
limits follow `rate_limits.fail_mode`, open by default.

Commands below assume the compose stack (`cd deploy`) and the admin token in
`$SWITCHYARD_ADMIN_TOKEN` (`. ./.env` loads it). Useful everywhere:

```sh
curl -s -H "Authorization: Bearer $SWITCHYARD_ADMIN_TOKEN" 127.0.0.1:9090/admin/v1/providers | jq .  # breakers, cooldowns, in-flight, budget store
curl -s localhost:8080/readyz                                    # readiness and the config version
docker compose logs --no-log-prefix --since 15m gateway | jq -c 'select(.level != "INFO")'
```

`--no-log-prefix` matters: without it compose prefixes every line with
`gateway-1  | ` and `jq` cannot parse it.

**Recreating the gateway.** A container's environment is fixed when it is
created, so a change to `.env` needs `docker compose up -d
--force-recreate gateway`, as several steps below say. If you started the
stack with the observability overlay (`--env-file .env --env-file
observability.env`, or `make compose-up PROFILES=observability`), pass
the same `--env-file` flags and `--profile observability` to every such
command, or the recreated gateway loses the overlay and tracing goes off:

```sh
docker compose --profile observability --env-file .env --env-file observability.env up -d --force-recreate gateway
```

Or set `SWITCHYARD_TRACING_ENABLED=true` in `.env` once, and the plain
commands keep it.

Logs are JSON. Every request line carries `request_id` and, when a trace
context exists, `trace_id`; search Jaeger by it. Logs never contain
prompts, responses, API keys, the admin token, DSNs or Redis passwords.

## Contents

- Dependencies: [Cache Redis down](#cache-redis-down) ·
  [State Redis down (budgets and rate limits)](#state-redis-down-budgets-and-rate-limits) ·
  [Postgres down](#postgres-down) ·
  [Embedding deployment down](#embedding-deployment-down)
- Providers: [Provider outage and open breakers](#provider-outage-and-open-breakers) ·
  [Every candidate failing: 503 no_healthy_upstream](#every-candidate-failing-503-no_healthy_upstream) ·
  [Provider key rejected](#provider-key-rejected)
- Money: [Budget exhausted](#budget-exhausted) ·
  [Usage events dropped](#usage-events-dropped)
- The gateway: [Config reload failed](#config-reload-failed) ·
  [Gateway overhead high](#gateway-overhead-high) ·
  [Log lines dropped](#log-lines-dropped) ·
  [Trace spans dropped](#trace-spans-dropped) ·
  [Gateway down](#gateway-down) ·
  [Clients rate-limited](#clients-rate-limited)
- Security: [A key leaked](#a-key-leaked)

## Cache Redis down

**Symptom.** Responses that used to be cache hits are slower and cost
money again; `X-Switchyard-Cache` says `miss` for repeated requests.

**Signal.** `SwitchyardCacheErrorRateHigh` (layer `exact` or `semantic`);
`switchyard_cache_lookups_total{result="error"}` and
`switchyard_cache_writes_total{result="error"}` rise; the gateway logs a
warning at most every 10 s per layer. "Cache hit rate" panel drops.
`/readyz` stays 200: the cache is never a readiness dependency.

**Impact.** None on correctness. Every failed or timed-out lookup
(`cache.lookup_timeout`, default 25 ms; semantic `semantic_cache.lookup_timeout`,
150 ms) is served as a miss, so requests go upstream: cost rises by your
hit rate, and latency by at most the lookup timeout. Cached content is not
lost unless Redis lost it (the cache instance has no persistence).

**What to do.**

1. `docker compose ps redis-cache` and `docker compose logs redis-cache`.
   Out of memory is not an outage (it evicts, `allkeys-lru`); a crash or a
   network problem is.
2. Restart it: `docker compose restart redis-cache`. The gateway
   reconnects by itself; the cache refills as requests miss.
3. If it is slow rather than down (lookups at the timeout), check its CPU
   and `maxmemory`, and the semantic index size.

## State Redis down (budgets and rate limits)

Also the answer for **the rate-limit store being down**: rate limits,
budgets and the tenants' cache namespace versions all live in
`redis-state`.

**Symptom.** With the default fail modes, nothing visible to clients:
requests are served. With `fail_mode: closed`, clients get `503
budget_unavailable` or `503 rate_limit_unavailable`.

**Signal.** `SwitchyardStateStoreErrors` (critical);
`switchyard_budget_store_errors_total` and
`switchyard_ratelimit_decisions_total{result="error"}` rise ("State store
errors" panel); `GET /admin/v1/providers` reports
`"budget_store": {"status": "unavailable", ...}`; warnings in the log.
With `budgets.fail_mode: closed`, `/readyz` fails its `budget_store` check.

**Impact.**

- `budgets.fail_mode: open` (default): requests are served **without
  budget checks**. Per-request cost caps (`max_cost_per_request_usd`)
  still apply. Spend during the outage is still recorded in the usage
  ledger, but it is not added to the Redis budget counters, so budgets
  under-count it.
- `rate_limits.fail_mode: open` (default): requests are served **without
  rate or concurrent-stream limits**.
- `closed` for either: those requests get 503 until Redis answers.
- A failing store is marked unavailable for a few seconds after each
  failure, so requests do not each wait for it; then one request probes it
  again.
- Cache namespaces use the last version each replica knew, so a cache
  purge made during the outage may not take effect until it ends.
- The per-IP auth-failure limit is unaffected: it is in each replica's
  memory, never in Redis.

**What to do.**

1. `docker compose ps redis-state`, `docker compose logs redis-state`. It
   runs with `noeviction`: if it is out of memory, writes fail instead of
   evicting. Raise `maxmemory` or the host's memory; do not switch it to
   an evicting policy (an evicted namespace version brings purged cache
   entries back, and an evicted budget counter resets a budget).
2. Restart it: `docker compose restart redis-state`. The append-only file
   brings budgets and limits back.
3. If an outage was long and budgets matter, compare the ledger's spend
   (`GET /admin/v1/usage?group_by=day`) with the budgets; the counters do
   not backfill.
4. If you cannot accept unenforced spend during an outage, set
   `budgets.fail_mode: closed` (and reload): availability for money.

## Postgres down

**Symptom.** None for clients. Usage reports (`/switchyard/v1/usage`,
`/admin/v1/usage`) fail or miss the latest requests.

**Signal.** `SwitchyardUsageLedgerWriteErrors`;
`switchyard_usage_write_errors_total` rises and `switchyard_usage_queue_depth`
grows ("Usage ledger" panel). If it lasts long enough to fill the queue,
`SwitchyardUsageEventsDropped` follows.

**Impact.** Requests are unaffected: usage events are queued in memory
(`storage.ledger.queue_size`, default 100,000 per replica) and written when
Postgres is back, retried with backoff and never stored twice. A restart
of the gateway while Postgres is down flushes what it can within the
shutdown's bound (see [Gateway down](#gateway-down)) and drops the rest
(counted). A gateway **starting**
while Postgres is down exits: the ledger applies its migrations at
startup.

**What to do.**

1. `docker compose ps postgres`, `docker compose logs postgres`; check
   disk space first (`df` on the volume host).
2. Restart it: `docker compose restart postgres`. Watch the queue depth
   drain to 0.
3. Estimate the headroom: queue size divided by your request rate is how
   long an outage can last before events drop (100,000 at 100 requests/s
   is about 16 minutes). Raise `storage.ledger.queue_size` (restart
   required) if that is too short; queued events cost memory.
4. Do not restart gateways while the queue is deep unless you must.

## Embedding deployment down

**Symptom.** Near-duplicate requests stop being served from the semantic
cache; exact repeats still hit.

**Signal.** `switchyard_cache_lookups_total{layer="semantic",result="error"}`;
`SwitchyardCacheErrorRateHigh{layer="semantic"}`; upstream errors for the
embedding deployment (`text-embedding-3-small@openai`, or
`bge-small-en-v1.5@tei` with the tei profile) in
`switchyard_upstream_requests_total`.

**Impact.** The semantic cache is skipped (a miss), at most
`semantic_cache.lookup_timeout` (150 ms) added per eligible request while
lookups time out. Nothing else changes.

**What to do.** With TEI: `docker compose --profile tei ps tei` and its logs
(first start downloads the model; it is not ready until then). With a
provider: see [Provider outage and open breakers](#provider-outage-and-open-breakers).
To stop paying the timeout during a long outage, set
`SWITCHYARD_SEMANTIC_CACHE=false` and recreate the gateway, or disable
`cache.semantic` in the policy and reload.

## Provider outage and open breakers

**Symptom.** One provider is failing or slow. Clients mostly still get
answers, from other deployments, sometimes slower; `X-Switchyard-Route-Reason`
shows `fallback:<kind>` and `X-Switchyard-Attempts` is above 1.

**Signal.** `SwitchyardProviderErrorRateHigh` (more than 5 % of attempts
failing for 5 minutes) and `SwitchyardCircuitBreakerOpen` (a breaker open
at least once every 3 minutes for 5 minutes: its probes keep failing; a
breaker that tripped and then got no traffic sits half-open, without
alerting, until a request probes it); "Upstream errors by provider", "Breaker states" and
"Fallbacks and retries" panels; `GET /admin/v1/providers` shows each
deployment's breaker state and next transition.

**Impact.** A deployment's breaker opens after 5 consecutive failures or a
50 % failure ratio over 20 requests in 30 s; the router then skips it and
uses the next cheapest eligible deployment, so cost can rise (routing
savings can go negative) and quality can differ. After 30 s it lets 3
probe requests through: one success closes it. 429s start a short
cooldown instead of counting toward the breaker. Requests pinned to that
provider (`model@provider`), or whose policy allows nothing else, fail.

**What to do.**

1. Check the provider's status page. Look at the outcome label: `timeout`
   and `transport` point at the network or the provider's capacity,
   `server_error` at the provider, `rate_limited` at your quota,
   `auth` at your key (see [Provider key rejected](#provider-key-rejected)),
   `not_found` at a model ID in the catalog the provider no longer serves.
2. Nothing needs to be done for routing: breakers close by themselves.
3. If the provider will be down for long, or is degrading quality, deny it
   for a while: add `"*@<provider>"` to the policy's `deny`, then reload.
   Remove it afterwards.
4. For `rate_limited`, raise your quota with the provider, or lower
   traffic to it with `max_concurrency` on the provider.

## Every candidate failing: 503 no_healthy_upstream

**Symptom.** Clients get `503` with code `no_healthy_upstream`; the error
lists the attempts.

**Signal.** "Error ratio (5xx)" panel; `switchyard_requests_total{status_class="5xx"}`;
usually `SwitchyardCircuitBreakerOpen` for every deployment of the
requested model, or `SwitchyardProviderErrorRateHigh` for several
providers.

**Impact.** Those requests fail. Other models may be fine.

**What to do.**

1. `GET /admin/v1/providers`: are all of the model's deployments open or
   cooling down? Then it is a provider outage (above) on every provider of
   that model, or your network: test egress from the gateway host to the
   providers.
2. If it started with a config change, check the policy's `allow`, `deny`
   and `require_tags` (`switchyard route explain` shows how a request
   would be routed, offline) and roll back with a reload.
3. Tell clients that use a pinned `model@provider` to use the model name
   or `auto`, which can fall back.

## Provider key rejected

**Symptom.** Calls to one provider fail with 401 or 403, and fall back to
other providers.

**Signal.** `SwitchyardUpstreamAuthFailures`;
`switchyard_upstream_requests_total{outcome="auth"}`.

**Impact.** That provider is effectively out (its breakers open); traffic
moves to the others, at their price.

**What to do.** The key is missing, wrong, revoked or out of credit. With
the compose stack, a provider whose key is not in `.env` gets an empty key:
either set it, or keep `auto` off that provider with `SWITCHYARD_ALLOW`
(for example `*@openai`). Fix the key in `.env` and run
`docker compose up -d --force-recreate gateway`; with `${file:...}`
secrets, replace the file and reload.

## Budget exhausted

**Symptom.** Clients of one tenant or key get `429` with code
`budget_exceeded` and a `Retry-After` until the window resets (the next
UTC day or month). Before that, at `soft_at`, `auto` requests move to the
budget's `soft_profile` (`X-Switchyard-Route-Reason: budget_downgrade`).

**Signal.** `SwitchyardBudgetAbove90Percent` (from
`switchyard_budget_utilization_ratio{scope}`); `switchyard_budget_rejections_total`;
`switchyard_budget_notifications_total` (soft threshold reached, and
requests admitted over a `notify` budget); "Budget utilization" panel.

**Impact.** With `on_exhausted: block`, requests that would exceed the
limit are rejected until the window resets. With `notify`, they are served
and counted. Budgets admit on estimated cost and settle on actual cost, so
utilization can end slightly above 1.

**What to do.**

1. Decide whether the spend is legitimate: `GET
   /admin/v1/usage?tenant=<id>&group_by=key,model` shows who and what.
2. Legitimate: raise `limit_usd`, or move the tenant to a cheaper
   profile. In the compose stack the limit comes from
   `SWITCHYARD_MONTHLY_BUDGET_USD` in `.env`: change it there and recreate
   the gateway (`docker compose up -d --force-recreate gateway`, with the
   `--env-file` flags of the note at the top if you use the overlay). A
   reload alone does not apply it:
   it reads the file again, but the container's environment still holds
   the old value. Or, without a restart, replace
   `${SWITCHYARD_MONTHLY_BUDGET_USD:-100}` in
   `deploy/config/switchyard.yaml` with the new amount and reload.
3. Not legitimate (a runaway job, a leaked key): leave the budget in place
   and see [A key leaked](#a-key-leaked).

## Usage events dropped

**Symptom.** Usage reports and savings are missing requests.

**Signal.** `SwitchyardUsageEventsDropped` (critical, any increase of
`switchyard_usage_events_dropped_total`). Usually preceded by
`SwitchyardUsageLedgerWriteErrors` and a full `switchyard_usage_queue_depth`.

**Impact.** The dropped events are gone: the ledger, reports, verified
savings and per-tag attribution undercount. Budgets are not affected
(they are counted in Redis at request time), and neither are clients.
Events are also counted as dropped when the database rejects them or when
a shutdown cannot flush them in time (the gateway then exits with status
1).

**What to do.** Fix the cause, usually [Postgres down](#postgres-down) or
Postgres too slow for the write rate (check its CPU, disk and locks).
Raise `storage.ledger.queue_size` for more headroom, or
`storage.ledger.batch_size` for throughput (restart required). The
Prometheus counters `switchyard_cost_usd_total` and
`switchyard_tokens_total` were still counted, so they bound what was lost.

## Config reload failed

**Symptom.** A config change does not take effect. `POST /admin/v1/reload`
answers `400 {"result":"error","error":"..."}`.

**Signal.** `SwitchyardConfigReloadFailed`;
`switchyard_config_reloads_total{result="error"}`; a log line `msg="audit"`
with `action="config.reload"` and `result="error"`, and the validation
messages (file, line and path; never secret values). `/readyz` keeps
reporting the old `config_version`.

**Impact.** None: the previous config keeps serving, in full. Replicas may
now differ (compare `config_version` across `/readyz` responses).

**What to do.**

1. Read the error, from the reload response or the log, or run the same
   checks offline: `switchyard config validate -c switchyard.yaml` (with
   the same environment).
2. Fix the file and reload again. A required `${VAR}` that is not set in
   the gateway's environment cannot be fixed by a reload: set it in `.env`
   and recreate the container.
3. A reload that succeeds but reports `restart_required` applied
   everything else; restart to apply the fields it lists. They are the
   listeners and every other `server.*` setting except `admin_token`
   (TLS included), `storage`, `logging`, `telemetry`, `evaluation`,
   `cache`, `semantic_cache`, the health tracker's settings
   (`routing.breaker`, `routing.rate_limit_cooldown`,
   `routing.fallback.retry_budget_*`), and features a reload turns on that
   are built only at startup: the semantic cache
   (`policies[].cache.semantic`), shadow sampling
   (`policies[].shadow.sample_rate`) and, with `storage.mode: redis`, a
   response cache when none was on at startup (`policies[].cache`).
   Everything else applies at once: tenants and keys, the admin token,
   policies, the catalog, aliases, routing, budgets, cache policies, rate
   limits (`rate_limit` and `rate_limits.*`) and quality overrides.

**Startup-only parts.** The semantic cache's embedder and the shadow
runner's judge and comparison calls keep the adapters built at startup,
so a provider key rotated by a reload (through `${file:...}`) reaches
them only after a restart; the request path uses the new key at once. A
deployment a reload removes stays listed, with its last health state, in
`GET /admin/v1/providers` until a restart; it is never chosen.

## Gateway overhead high

**Symptom.** Requests are slower than the upstream explains.

**Signal.** `SwitchyardOverheadP99High` (overhead p99 above 25 ms for 10
minutes; the SLO is p50 ≤ 2 ms and p99 ≤ 10 ms); "Overhead" panels;
`switchyard_overhead_duration_seconds`.

**Impact.** Added latency on every request.

**What to do.**

1. CPU: is the gateway container throttled or the host saturated? Add
   replicas or CPU.
2. Dependencies on the request path: state Redis latency (budgets and rate
   limits are checked per request) and cache lookups ("Cache lookup
   latency p99" panel). A slow Redis shows here before it fails.
3. Logging: debug level, or [Log lines dropped](#log-lines-dropped),
   means the log sink is a bottleneck.
4. Very large requests (bodies near `server.max_request_bytes`) cost
   decoding time; compare by endpoint.

## Log lines dropped

**Symptom.** Gaps in the gateway's logs.

**Signal.** `SwitchyardLogLinesDropped`;
`switchyard_log_lines_dropped_total`.

**Impact.** The access log is written asynchronously so that a slow log
sink never slows requests; when its queue is full, lines are dropped and
counted. Metrics and the usage ledger are complete; only log lines are
missing.

**What to do.** The log consumer (Docker's logging driver, a sidecar or
the terminal) is not keeping up. Use a faster driver (`local` or a
buffered shipper), drop the level to `info` if it is at `debug`, or
spread the load over more replicas.

## Trace spans dropped

**Symptom.** Traces in Jaeger (or your collector) are missing or have
gaps.

**Signal.** `SwitchyardTracingSpansDropped`;
`switchyard_tracing_spans_dropped_total` rises while
`switchyard_tracing_spans_exported_total` stalls; a rate-limited warning
in the log about the export (the collector's address is never logged).

**Impact.** None on requests: tracing fails open. Spans wait at most 5 s
in a bounded queue (2,048 spans) and are posted once, without retries;
a batch the collector refuses or does not answer within 10 s is dropped,
and so are new spans while the queue is full. Both count in
`switchyard_tracing_spans_dropped_total`.

**What to do.** Check that the collector at `telemetry.tracing.endpoint`
is up and reachable from the gateway (OTLP/HTTP protobuf, on
`<endpoint>/v1/traces`; with compose, `docker compose ps jaeger`). Its
credentials can only be the URL's userinfo, sent as Basic auth: headers
such as `OTEL_EXPORTER_OTLP_HEADERS` are not supported. Under sustained
high traffic, lower `telemetry.tracing.sample_ratio` (it needs a
restart).

## Gateway down

**Symptom.** Clients cannot connect, or get errors from your load
balancer.

**Signal.** `SwitchyardDown` (Prometheus cannot scrape the admin
listener); `docker compose ps` shows the gateway restarting or unhealthy.

**What to do.**

1. `docker compose logs --tail 100 gateway`. At startup the gateway
   validates its config and exits with the reasons when it is invalid, or
   when Postgres is unreachable (the ledger migrates at startup), or when
   a listener's port is taken.
2. Check its environment: a `${VAR}` without a default that is not set is
   a startup error naming the variable.
3. After SIGTERM the gateway fails `/readyz` and keeps serving for
   `server.shutdown_delay` (0 s by default), drains in-flight requests for
   up to `server.shutdown_grace` (60 s), then has at most 15 s for the
   rest: shadow samples and cache writes, then the usage queue's flush,
   then the span flush (at most 2 s) and the log drain (at most 2 s). So
   it exits within `shutdown_delay + shutdown_grace + 15s` of SIGTERM:
   75 s with the defaults, compose's `stop_grace_period`. Size
   Kubernetes' `terminationGracePeriodSeconds` the same way (plus your
   preStop hook, if any). A container killed earlier loses its queued
   usage events.

## Clients rate-limited

**Symptom.** Clients get `429` with code `rate_limit_exceeded` and a
`Retry-After` in whole seconds.

**Signal.** `switchyard_ratelimit_decisions_total{result="limited"}`
("Rate-limit decisions" panel), by `kind`: `request` (the key's
requests per minute) or `stream` (its concurrent streams).
`switchyard_ratelimit_auth_limited_total` counts client addresses limited
for repeated authentication failures (`rate_limits.auth_failures_per_minute`,
default 30): they get 429 whatever key they send.

**Impact.** Working as configured; cache hits count too.

**What to do.** Raise the key's or tenant's `rate_limit` and reload (it
applies at once: a raised rate within one new interval), or ask the
client to back off and honor `Retry-After`. With `storage.mode: redis`
the request rate and the stream cap are shared by every replica through
`redis-state`; in memory mode each replica has its own. A stream slot of
a replica that crashed frees itself within a minute (the lease TTL).

The auth-failure limit is per replica, in memory, by the connection's
address (IPv4, or the IPv6 /64): with n replicas an address can fail up to
n times the limit. It counts failed admin tokens as well as failed API
keys. Each attempt is checked and, if it failed, counted in one step, so
concurrent failed attempts cannot get past the limit; valid keys never
count against it. Once over it, an address gets 429 whatever it sends, so
a refused guess learns nothing. Switchyard never trusts `X-Forwarded-For`, so behind a
reverse proxy or load balancer that does not preserve the client address
every client shares the proxy's address and one client's failures lock
out everyone: set `rate_limits.auth_failures_per_minute: -1` there and
limit at the proxy instead. Many limited addresses at once suggest key
guessing; see [A key leaked](#a-key-leaked).

## A key leaked

### A gateway API key

A gateway key (`sy_...`) leaked into a repository, a log or a ticket.

1. **Revoke it now.** Delete its entry (by `id`; the config holds only its
   hash) from `tenants[].keys` and reload: `docker compose kill -s HUP
   gateway` or `POST /admin/v1/reload`. The reload is atomic: requests in
   flight finish, and from that moment the key gets 401. Check the reload
   response says `"result":"ok"`.
2. **Issue a replacement** with `switchyard keys generate --tenant <id>
   --key-id <new-id>`, add its entry, reload, and hand the key to its
   owner. To rotate without downtime, add the new key first, let the
   client switch, then remove the old one.
3. **Assess the damage**: `GET /admin/v1/usage?tenant=<id>&group_by=key,day`
   shows what the old key spent, and the access log (`key_id`) when and
   from where.
4. If the tenant's budget was used up by the abuse, consider resetting
   expectations rather than the limit; see [Budget exhausted](#budget-exhausted).

### A provider key

1. Revoke it at the provider and create a new one.
2. Put the new key where the gateway reads it (`.env`, or the secret file
   behind `${file:...}`). An environment variable needs a container
   restart (`docker compose up -d --force-recreate gateway`); a secret
   file needs only a reload.
3. Until then, the provider answers 401: see
   [Provider key rejected](#provider-key-rejected). Traffic falls back to
   other providers.
4. The semantic cache's embedding calls and shadow sampling keep the key
   they started with until a restart (see
   [Config reload failed](#config-reload-failed)): restart the gateway if
   either uses that provider.

### The admin token

Generate a new one (for example with `switchyard keys generate`, using the
printed key; it must be at least 32 characters, with no whitespace), set
`SWITCHYARD_ADMIN_TOKEN` in `.env`, and recreate the
gateway (`docker compose up -d --force-recreate gateway`): the container
reads its environment only when it starts. With `${file:...}`, replace
the file and reload. Confirm the admin
listener was never published beyond loopback or your private network, and
check the audit log lines (`msg="audit"`) for reloads and cache purges you
did not make.
