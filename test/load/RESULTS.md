# Overhead benchmark results

## P7 acceptance soak: 1 hour at 1,000 req/s in Redis mode, 2026-11-19

`make soak DURATION=1h SOAK_FLAGS="-redis-url redis://127.0.0.1:26379/0"` against a throwaway `redis:8` container. This is the run behind PLAN P7's soak criterion.

- Hardware: Apple M5, 10 cores (4 performance, 6 efficiency), 16 GiB RAM, macOS 26.4.1 (arm64), go1.27.4
- Load average (1, 5, 15 min): 6.90 6.04 5.86 before, 7.58 8.87 8.76 after
- Load: 1000 req/s open loop for 1h0m0s; 10% cacheable repeats, 20% of the rest streaming; upstream latency 20ms; 2.00% injected upstream 500s (seed 1); retries and fallback off
- Gateway: Redis state; key rate limit 1000000 rpm, burst 100000, 640 concurrent streams (never reached); tracing on, sample_ratio 1, to an in-process OTLP collector
- Error rate of the uncached requests (information): 64727 in 3240570 = 1.997%; 99% Wilson interval [1.977%, 2.018%]; configured 2.000%
- Flatness compares the median of the middle part (20m0s to 40m0s) with the last (40m0s to 1h0m0s), after a 20m0s warm-up; 120 samples every 30s
- Requests: 3600000 sent, 3600000 completed, 359430 cache hits, 648000 streams, 64727 errors
- Log lines dropped by the gateway's asynchronous writer (switchyard_log_lines_dropped_total, last sample): 0
- Upstream: 3240570 requests, 64727 injected 500s; the client saw 64727 errors (equal when every error is an injected one)
- Errors by kind: 502: 64727
- Distinct first errors: `status 502: {"error":{"message":"The upstream provider returned HTTP 500.","type":"server_error","param":null,"code":"upstream_error"}}`

| check | result | detail |
|---|---|---|
| goroutines | pass | median 237 in the middle part, 238 in the last (limit 409, +0.4%) |
| RSS | pass | peak 230.2 MiB in the middle part, 244.5 MiB in the last (limit 264.7 MiB, +6.2%) |
| heap (GC goal) | pass | median 205.2 MiB in the middle part, 210.1 MiB in the last (limit 236.0 MiB, +2.4%) |
| errors | pass | the client saw 64727 errors, the upstream injected 64727 (must be equal) |
| spans | pass | 31321600 exported, 0 dropped by the end of the load; the collector received 31321831 in all, rejected 0 exports |
| rate limits | pass | 3599984 requests allowed, 647998 stream leases taken, 0 refused, 18 store errors |
| log lines | pass | 0 dropped by the asynchronous writer |

**PASS**

The GC goal rose from about 206 MiB to 223 MiB in the last 10 minutes, as the machine's load average climbed past 8. Goroutines stayed flat, and every gate passed. The 18 rate-limit store errors were Redis calls that timed out on the loaded machine; `fail_mode: open` served those 16 requests and 2 streams.

## P7 review fixes: production-shaped SLO and soak at target load, 2026-11-19

**Target load is 1,000 req/s**, the SLO's rate: the soak's default `-rate`
since the review fixes. Both benches now run a production-shaped hot path:
a memory-mode (or, for the soak with `-redis-url`, Redis) per-key rate
limit and `max_concurrent_streams` far above the load, so every request
takes the limiter's path and every stream a lease without one being
refused, and tracing on with `sample_ratio: 1`, every span exported to an
in-process OTLP collector (`loadkit.Collector`). The earlier sections below
ran with none of this.

Hardware as documented below (Apple M5, 10 cores, 16 GiB, macOS 26.4.1,
go1.27.4; Colima's VM running beside). Load averages per run.

### SLO (`make bench-slo BENCH_SLO_FLAGS="-bin bin/switchyard"`), three runs

Open loop at 1,000 req/s for 60 s after a 5 s warmup, 80% non-streaming,
20% streaming, caching off, fake upstream 20 ms to the first byte.

| run | load avg before → after | gateway added p50 | p90 | p99 | max | generator lag p99 (max) | spans received | result |
|---|---|---:|---:|---:|---:|---:|---:|---|
| 1 | 3.76 4.48 4.11 → 4.85 4.60 4.21 | 0.292ms | 0.400ms | 1.004ms | 66.7ms | 0.196ms (19.5ms) | 455,000 | PASS |
| 2 | 4.85 4.60 4.21 → 4.66 4.49 4.21 | 0.266ms | 0.367ms | 4.527ms | 45.6ms | 0.783ms (25.5ms) | 455,000 | PASS |
| 3 | 4.37 4.43 4.19 → 4.92 4.79 4.38 | 0.264ms | 0.377ms | 2.787ms | 58.3ms | 0.491ms (42.5ms) | 455,000 | PASS |

All three pass the gates (p50 ≤ 2 ms, p99 ≤ 10 ms) with zero errors. The
collector received exactly 7 spans for each of the 65,000 requests
(warmup included) in every run: none dropped. The p99 is higher than in
the untraced runs below (1.0–4.5 ms against 0.9–1.9 ms) and moves more
between runs: full tracing and the limiter cost tail latency, within the
gate. p50 is unchanged (0.26–0.29 ms).

### Soak, 10 minutes in memory (`make soak DURATION=10m`, default 1,000 req/s)

Load average 4.17 4.62 4.33 at the start, 7.42 7.21 5.89 at the end
(the box was otherwise idle). Warm-up 3m20s. **PASS**:

| check | middle (3m20s–6m40s) | last (6m40s–10m) | limit / gate |
|---|---:|---:|---:|
| goroutines, median | 228 | 220 | 399 |
| RSS, peak | 216.3 MiB | 217.1 MiB | 248.7 MiB |
| heap GC goal, median | 191.8 MiB | 190.1 MiB | 220.5 MiB |
| errors = injected | | 10,892 = 10,892 | equal |
| spans dropped | | 0 (5,219,840 exported by the end of the load; 5,220,045 received in all) | 0 |
| rate-limit refusals | | 0 (600,000 requests allowed, 108,000 stream leases) | 0 |
| log lines dropped | | 0 | 0 |

600,000 requests, 59,992 cache hits, 108,000 streams; error rate of the
uncached requests 2.017% (99% Wilson interval 1.968–2.067%, configured
2.000%), every error a 502 for an injected upstream 500.

### Soak, 5 minutes in Redis mode (`-redis-url`, a redis-state of compose project `p7fix-soak`)

`storage.mode: redis` on one Redis 8.10 (state and cache roles), so the
Redis limiter, its stream leases and heartbeats, the circuit and the cache
client ran under load. Load average 5.55 6.74 5.78 → 8.19 7.35 6.33.
Warm-up 1m50s. **PASS**:

| check | middle (1m50s–3m25s) | last (3m25s–5m) | limit / gate |
|---|---:|---:|---:|
| goroutines, median | 237 | 235 | 409 |
| RSS, peak | 220.6 MiB | 222.6 MiB | 253.7 MiB |
| heap GC goal, median | 198.6 MiB | 202.1 MiB | 228.3 MiB |
| errors = injected | | 5,381 = 5,381 | equal |
| spans dropped | | 0 (2,609,664 exported; 2,610,152 received) | 0 |
| rate-limit refusals / store errors | | 0 / 0 (300,000 allowed, 54,000 stream leases) | 0 |
| log lines dropped | | 0 | 0 |

Five minutes is a smoke run of the Redis path, not a leak test; the 1-hour
acceptance soak should be run again with this build at the default rate.

## P7 integration (wave 2): SLO, overhead A/B and soak, 2026-11-18

The integrated build: tracing (disabled in these benches: the bench
configs turn it off, so the spans cost nothing; see
`TestStartUntracedAllocs`), rate limiting (the bench keys have no limits,
so the enforcer only reads the key's Principal), the asynchronous log
writer and the reload machinery.

**Documented hardware.** Apple M5, 10 cores (4 performance, 6
efficiency), 16 GiB RAM, macOS 26.4.1 (arm64), go1.27.4. The machine is a
shared development laptop with Colima's VM (4 CPUs, 6 GiB) running beside
the benchmarks; it was otherwise idle apart from this work. Load averages
are given per run (1-minute, 5-minute, 15-minute).

### SLO (`go run ./test/load slo -bin bin/switchyard`, what `make bench-slo` runs)

Open loop at 1,000 req/s for 60 s after a 5 s warmup, 80% non-streaming,
20% streaming, caching off, fake upstream with 20 ms to the first byte.
Runs 1–3 are the requested three; 4 and 5 were added after run 3's stall.

| run | load avg at start | gateway added p50 | p90 | p99 | generator lag p99 (max) | result |
|---|---|---:|---:|---:|---:|---|
| 1 | 3.90 5.40 5.55 | 0.331ms | 0.581ms | 1.743ms | 0.775ms (38.5ms) | PASS |
| 2 | 6.54 5.74 5.65 | 0.320ms | 0.577ms | 1.921ms | 0.770ms (16.5ms) | PASS |
| 3 | 3.81 4.84 5.29 | 0.319ms | 0.615ms | 46.479ms | 5.769ms (153.6ms) | FAIL (stall warning) |
| 4 | 4.73 4.74 5.17 | 0.279ms | 0.372ms | 0.879ms | 0.152ms (7.4ms) | PASS |
| 5 | 4.04 4.46 4.99 | 0.273ms | 0.392ms | 1.208ms | 0.329ms (13.1ms) | PASS |

Run 3's p99 of 46 ms came with the load generator itself 5.8 ms late at
p99 and up to 154 ms late (in every other run under 1 ms at p99); the
report flagged it as a machine stall, and its non-streaming and streaming
p99 were equal (46.2 and 46.6 ms), as for a process-wide pause rather than
a slow path. Four of five runs pass with p99 0.9–1.9 ms against the 10 ms
gate, and p50 0.27–0.33 ms against 2 ms in all five.

### Overhead A/B (`make bench-overhead` flags, `-bin`), alternating with HEAD 7348ee9

Closed loop, non-stream, concurrency 100, 10 s after a 2 s warmup,
zero-latency fake.

| round | build | load avg at start | added p50 | p90 | p99 | gateway req/s |
|---|---|---|---:|---:|---:|---:|
| 1 | HEAD 7348ee9 | 4.74 4.31 4.82 | 0.895ms | 2.123ms | 6.747ms | 50,333 |
| 1 | P7 | 9.91 5.55 5.25 | 0.956ms | 2.365ms | 7.991ms | 48,767 |
| 2 | HEAD 7348ee9 | 11.96 6.38 5.56 | 0.988ms | 2.361ms | 7.880ms | 48,092 |
| 2 | P7 | 13.27 7.02 5.81 | 0.940ms | 2.235ms | 7.594ms | 49,798 |
| 3 | HEAD 7348ee9 | 14.77 7.84 6.14 | 0.991ms | 2.445ms | 7.578ms | 47,962 |
| 3 | P7 | 14.68 8.37 6.38 | 0.914ms | 2.253ms | 7.741ms | 49,894 |

Both builds miss this benchmark's 5 ms p99 gate in every run, as every
build has on this machine since P5: the closed loop at concurrency 100
saturates the CPU (load climbs from 5 to 15 during the six runs), so its
tail is queueing. Between the builds the differences are within the
run-to-run noise in both directions: P7 is better in two of three pairs at
p50 and p90 and serves more requests per second in two of three, HEAD is
better in two of three at p99. The open-loop SLO above is the P7
criterion.

### Soak (`make soak DURATION=10m SOAK_FLAGS="-rate 500"`)

A 10-minute smoke run of the integrated build; the 1-hour acceptance run
is separate. `make soak DURATION=10m` alone exits with a usage error at
the default 200 req/s: its warm-up, until the memory ledger holds 100,000
events, is 9m10s, which leaves too little of a 10-minute run (the Makefile
and README now say so). Load average 8.90 at the start, 4.94 at the end.
**PASS**:

| check | middle (3m40s–6m50s) | last (6m50s–10m) | limit |
|---|---:|---:|---:|
| goroutines, median | 71 | 71 | 130 |
| RSS, peak | 211.2 MiB | 210.9 MiB | 242.8 MiB |
| heap GC goal, median | 183.0 MiB | 186.5 MiB | 210.4 MiB |

300,000 requests, 29,992 cache hits, 54,000 streams; 5,381 errors out of
270,008 uncached requests = 1.993% (99% Wilson interval 1.925–2.063%,
injected 2.000%), every one a 502 for an injected upstream 500. No log
line was dropped.

## P7: SLO bench (`make bench-slo`), 2026-11-18

Gate (docs/PLAN.md, P7): at 1,000 req/s on documented hardware, the gateway
adds at most 2 ms at p50 and 10 ms at p99.

- Hardware: Apple M5, 10 cores (4 performance, 6 efficiency), 16 GiB RAM,
  macOS 26.4.1 (arm64), go1.27.4. The machine was shared with four other
  engineers' builds and tests; load averages of 4–7 during these runs, and
  20–50 earlier in the day.
- Open loop at 1,000 req/s for 60 s after a 5 s warmup; 80% non-streaming,
  20% streaming with usage; caching off; gateway log level info; fake
  upstream with 20 ms to the first byte and 1 ms between stream events.
- Added latency per request = client latency minus the upstream's own time
  for that request (see the package doc of test/load).
- `base` is HEAD 7348ee9; `after` is HEAD plus the asynchronous log writer
  (internal/telemetry.AsyncWriter, wired into internal/app as in the P7
  integration note). Runs alternated base, after, base, ….

| run | build | gateway added p50 | p90 | p99 | generator lag p99 | result |
|---|---|---:|---:|---:|---:|---|
| 1 | base | 0.288ms | 0.467ms | 1.776ms | 0.544ms | PASS |
| 1 | after | 0.281ms | 0.403ms | 1.222ms | 0.328ms | PASS |
| 2 | base | 0.298ms | 0.495ms | 1.480ms | 0.477ms | PASS |
| 2 | after | 0.269ms | 0.420ms | 1.167ms | 0.266ms | PASS |
| 3 | base | 0.327ms | 0.532ms | 0.972ms | 0.184ms | PASS |
| 3 | after | 0.306ms | 0.546ms | 10.392ms | 3.846ms | FAIL (stall) |

Directly against the fake, the same subtraction gives p50 0.12–0.19 ms and
p99 0.6–1.0 ms: the cost of one loopback hop and the client.

Five of six runs pass with a wide margin (p99 1–1.8 ms against 10 ms). Run 3
of `after` missed the p99 gate by 0.4 ms, and in that run the load
generator itself fell behind its schedule (lag p99 3.8 ms, max 80 ms,
against under 0.6 ms in every other run): the process was starved of CPU
for stretches, which shows up as tail latency on every hop. The report now
prints a warning when the generator lag p99 exceeds 2 ms. On this evidence
the asynchronous writer improves the p99 slightly at this rate (1.17–1.22
ms against 1.48–1.78 ms in the paired runs 1 and 2), but the difference is
within what this shared machine can resolve; the SLO holds either way.

## P7: asynchronous access log, `make bench-overhead` A/B, 2026-11-18

The closed-loop overhead benchmark (default flags: non-stream,
concurrency 100, 10 s after a 2 s warmup, zero-latency fake), same `base`
and `after` binaries as above, run alternately three rounds of three
pairs. The P5 finding was that the synchronous access-log write, made under
the JSON handler's mutex, dominated the gateway's CPU profile; the
`after` build queues each encoded line for a background goroutine instead,
which writes them in batches of up to 64 KiB.

| round (load avg) | build | run 1 p50 / p99 added | run 2 | run 3 |
|---|---|---|---|---|
| 1 (40–50) | base | 1.210 / 8.802ms | 1.326 / 11.738ms | 1.144 / 8.043ms |
| 1 (40–50) | after | 1.283 / 11.807ms | 1.079 / 7.505ms | 1.332 / 18.877ms |
| 2 (9–23) | base | 1.316 / 11.244ms | 1.157 / 9.477ms | 1.320 / 14.595ms |
| 2 (9–23) | after | 1.181 / 12.491ms | 1.151 / 12.860ms | 1.425 / 16.644ms |
| 3 (6–14) | base | 0.926 / 7.760ms | 0.854 / 5.894ms | 0.939 / 8.582ms |
| 3 (6–14) | after | 0.826 / 5.897ms | 0.816 / 6.345ms | 0.878 / 6.974ms |

Gateway throughput in round 3: base 49.9k, 55.9k and 48.4k req/s; after
56.3k, 56.5k and 52.0k req/s.

Reading: on the quietest round, `after` is better in every pair at p50
(0.82–0.88 ms against 0.85–0.94 ms), better in two of three pairs at p99
(mean 6.4 ms against 7.4 ms), and serves 3–14% more requests per second.
In the two noisier rounds the machine's own load (load averages of 9–50,
from the other engineers' work on top of this CPU-saturating benchmark)
swamps any difference in either direction. The 5 ms p99 gate of this
benchmark was missed by both builds in every run today, as it was for every
build since P5 on this machine; this closed loop at concurrency 100
saturates the machine, so its tail is CPU queueing, not the gateway
(DESIGN §20; see the analysis below). The open-loop SLO bench above is the
P7 criterion, and it passes. A microbenchmark isolates the change itself
(`go test -bench Logger ./internal/telemetry`, 10 goroutines logging an
access-log-sized line to a file): 4.4–5.1 µs per line on the caller's
path synchronously against 1.1–1.3 µs through the AsyncWriter, with the
same allocations (3 per line) and no line dropped.

## P7: soak (`make soak`), 2026-11-18

Verification run of 10 minutes (the 1-hour acceptance run is separate):
`go run ./test/soak -duration 10m -rate 500` against the `after` binary,
same machine (load average 4–13). 500 req/s rather than the default 200 so
that the memory usage ledger (100,000 events) fills within the warm-up of a
10-minute run; at 200 req/s the auto warm-up is 9.2 minutes, which only a
longer run leaves room for. **PASS**:

| check | middle (3m40s–6m50s) | last (6m50s–10m) | limit |
|---|---:|---:|---:|
| goroutines, median | 70 | 70 | 129 |
| RSS, peak | 207.9 MiB | 208.1 MiB | 239.0 MiB |
| heap GC goal, median | 181.7 MiB | 186.0 MiB | 208.9 MiB |

300,000 requests, 29,992 cache hits, 54,000 streams; 5,381 errors out of
270,008 uncached requests = 1.993% (99% Wilson interval 1.925–2.063%,
injected 2.000%), every one a 502 for an injected upstream 500, equal to the
fake's count of injected errors. No log line was dropped.

Two trial runs shaped the criteria (see test/soak's package doc): with an
unbounded client connection pool the goroutine count followed the
client's bursts (75 to 300), and the in-use heap and RSS samples swing with
GC and the scavenger (heap 116–189 MiB, RSS 100–209 MiB within a minute),
so the soak now keeps a fixed pool of 16 client connections and judges the
heap by the GC goal and RSS by its peak.

# Before P7

Target (docs/PLAN.md, P1, restated 2026-11-11): p99 added latency ≤ 5 ms with
100 concurrent non-streaming requests against a zero-latency fake upstream on a
single machine. This is the default `make bench-overhead` run.

Concurrency 200 was the original target. It is kept below as a reference
number: at that load, the load generator, the fake and the gateway saturate one
machine's CPU, so the result mostly measures CPU contention, not the gateway.
High-load measurement with the load generator off-box belongs to P7.

**Acceptance run, 2026-11-11** (`make bench-overhead`; Apple M5, 10 cores, Go 1.27.3; non-stream, concurrency 100, 10s after a 2s warmup): **PASS**.

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1390636 | 0 | 139064 | 0.547ms | 1.533ms | 2.664ms |
| gateway | 458922 | 0 | 45892 | 1.797ms | 4.230ms | 7.490ms |
| added | | | | 1.250ms | 2.697ms | 4.826ms |

The margin is thin (4.8 ms against 5 ms), and earlier runs at this concurrency measured 4.4–4.7 ms. A noisy machine can tip a single run over the threshold, so rerun before treating one failure as a regression.

**Result after the Phase 1 review fixes: still not met at concurrency 200**
(p99 added 8.2, 10.5 and 8.6 ms in three runs). Met at concurrency 100
(4.4 ms) and 50 (2.4 ms). Merging the access-log and completion lines into
one line per request made no difference that these runs can resolve: the
pre-fix binary, run alternately in the same session, measured 8.3, 9.1 and
8.5 ms. Streams at concurrency 200 did improve (p99 added 12.4 ms, against
20.1 ms for the pre-fix binary), because a finished stream now gives its
upstream connection back to the pool instead of dropping it.

## After the review fixes

Measured 2026-11-09 on the same machine and conditions as below, except that
the gateway now writes one log line per request. Each configuration ran the
fixed binary and the pre-fix binary (built from a copy of the tree before
the fixes) back to back, with the same load generator, so the two columns
share the machine's state. The pre-fix numbers in this section differ a
little from the earlier ones further down; that is run-to-run noise.

Commands: as listed under "Commands" below, with `-bin` pointing at each
binary.

### Non-stream, concurrency 200 (the acceptance run): FAIL

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1461349 | 0 | 146135 | 1.067ms | 2.880ms | 4.803ms |
| gateway | 528114 | 0 | 52811 | 3.209ms | 7.308ms | 13.006ms |
| added | | | | 2.142ms | 4.429ms | 8.203ms |

Two more runs gave p99 added 10.51 ms and 8.61 ms. The pre-fix binary in
the same session: 8.29 ms, 9.05 ms and 8.51 ms.

### Non-stream, concurrency 100: PASS

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1472253 | 0 | 147225 | 0.524ms | 1.434ms | 2.450ms |
| gateway | 498618 | 0 | 49862 | 1.658ms | 3.873ms | 6.839ms |
| added | | | | 1.134ms | 2.439ms | 4.389ms |

Pre-fix binary in the same session: p99 added 4.622 ms.

### Non-stream, concurrency 50: PASS

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1443736 | 0 | 144374 | 0.273ms | 0.695ms | 1.291ms |
| gateway | 430899 | 0 | 43090 | 0.955ms | 2.226ms | 3.704ms |
| added | | | | 0.683ms | 1.531ms | 2.413ms |

Pre-fix binary in the same session: p99 added 2.153 ms.

### Stream, concurrency 200: FAIL (not an acceptance criterion)

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 375664 | 0 | 37566 | 4.063ms | 11.876ms | 20.781ms |
| gateway | 173522 | 0 | 17352 | 10.406ms | 21.126ms | 33.182ms |
| added | | | | 6.343ms | 9.249ms | 12.402ms |

Pre-fix binary in the same session: 15752 req/s, p99 added 20.149 ms.

### Stream, concurrency 50: PASS

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 310200 | 0 | 31020 | 1.310ms | 3.289ms | 5.858ms |
| gateway | 152838 | 0 | 15284 | 2.935ms | 5.853ms | 9.293ms |
| added | | | | 1.625ms | 2.564ms | 3.434ms |

Pre-fix binary in the same session: 14812 req/s, p99 added 4.532 ms.

### What changed

- One log line per request instead of two. The earlier profile put the two
  log writes at about 16% of the gateway's CPU, and the earlier experiment
  of dropping both writes improved p99 only from about 9.5 ms to 8.6 ms.
  Dropping one of the two is within the noise of these runs.
- Finished streams reuse their upstream connection. Before, every stream
  closed its connection before reading the end of the response, so each
  stream opened a new upstream connection; that is the stream improvement.
- The threshold is unchanged (5 ms).

# Earlier results (after the Phase 1 integration, before the review fixes)

**Result then: not met at concurrency 200** (p99 added 9.5 ms). Met at
concurrency 100 (4.7 ms) and 50 (2.4 ms). The gateway's own time, measured
in-process, is p99 ≤ 0.25 ms; the rest of the added latency is the second
HTTP hop and CPU queueing on a machine that also runs the load generator and
the fake. Details below.

## Machine and conditions

- Apple M5, 10 cores, 16 GiB RAM, macOS 26.4.1, Go 1.27.3 (darwin/arm64).
- The load generator, the fake upstream (both in the `go run ./test/load`
  process) and the gateway (a separate process) share the one machine, over
  loopback. At concurrency 200 the machine is CPU-saturated in both phases.
- Closed loop: each worker sends its next request when the previous one
  finishes, for 10 s after a 2 s warmup. In a saturated closed loop, latency
  is mostly queueing: mean latency ≈ concurrency ÷ throughput (Little's law).
- Gateway log level info, logs discarded by the harness (the gateway still
  encodes and writes two lines per request: the access log and the
  completion line).
- Measured 2026-11-09, after the Phase 1 integration. (Since the review
  fixes the gateway writes one line per request; see above.)

## Commands

```sh
make bench-overhead                                         # non-stream, concurrency 200
make bench-overhead BENCH_FLAGS="-concurrency 100"
make bench-overhead BENCH_FLAGS="-concurrency 50"
make bench-overhead BENCH_FLAGS="-mode stream"              # stream, concurrency 200
make bench-overhead BENCH_FLAGS="-mode stream -concurrency 50"
```

## Results

### Non-stream, concurrency 200 (the acceptance run): FAIL

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1390758 | 0 | 139076 | 1.108ms | 3.040ms | 5.116ms |
| gateway | 489128 | 0 | 48913 | 3.386ms | 7.915ms | 14.610ms |
| added | | | | 2.277ms | 4.875ms | 9.494ms |

Repeated runs gave p99 added 9.58 ms, 9.48 ms, 8.07 ms and 8.36 ms.

### Non-stream, concurrency 100: PASS

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1395704 | 0 | 139570 | 0.549ms | 1.513ms | 2.616ms |
| gateway | 469151 | 0 | 46915 | 1.735ms | 4.201ms | 7.305ms |
| added | | | | 1.185ms | 2.688ms | 4.689ms |

### Non-stream, concurrency 50: PASS

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 1341822 | 0 | 134182 | 0.291ms | 0.748ms | 1.419ms |
| gateway | 414657 | 0 | 41466 | 0.983ms | 2.333ms | 3.809ms |
| added | | | | 0.692ms | 1.585ms | 2.390ms |

### Stream, concurrency 200: FAIL (not an acceptance criterion)

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 371100 | 0 | 37110 | 3.956ms | 12.442ms | 21.577ms |
| gateway | 158326 | 0 | 15833 | 11.087ms | 23.937ms | 40.448ms |
| added | | | | 7.131ms | 11.495ms | 18.871ms |

### Stream, concurrency 50: PASS

| target | requests | errors | req/s | p50 | p90 | p99 |
|---|---:|---:|---:|---:|---:|---:|
| direct | 305755 | 0 | 30576 | 1.368ms | 3.256ms | 5.517ms |
| gateway | 146038 | 0 | 14604 | 3.092ms | 6.184ms | 9.608ms |
| added | | | | 1.724ms | 2.929ms | 4.091ms |

## Analysis

**The gateway's own time.** `switchyard_overhead_duration_seconds` (request
time minus time waiting on the upstream), scraped from admin `/metrics` at
the start and end of the measured window of a non-stream concurrency-200
run, 445,688 requests:

| bucket | share of requests |
|---|---:|
| ≤ 0.05 ms | 98.15% |
| ≤ 0.25 ms | 99.18% |
| ≤ 1 ms | 99.63% |
| ≤ 5 ms | 99.99% |

So p50 and p90 ≤ 0.05 ms and p99 ≤ 0.25 ms inside the handler. This
histogram does not include net/http reading the request and writing the
response or the transport's own work, which is where the rest goes.

**CPU profile.** A CPU profile of the gateway during the concurrency-200
non-stream run (a build with net/http/pprof added for the measurement only)
showed the gateway using about 4.5 cores at 49k req/s:

- 83% of samples in read/write syscalls: about 34% on the client and
  upstream sockets' writes, 32% on their reads, and 16% writing the two log
  lines per request to stdout.
- The Switchyard handler code, excluding logging, is about 2% of samples.
  No JSON re-encoding, regexp, compression or allocation hot spot showed up;
  GC is a few percent.

Dropping the two per-request log writes (an experiment, not a change)
improved the concurrency-200 p99 added only from about 9.5 ms to 8.6 ms, and
giving the gateway more of the machine (`GOMAXPROCS=5` for the load process)
to 6.6 ms. Neither reaches 5 ms. The added latency at concurrency 200 is the
cost of a second HTTP hop on a saturated machine: the gateway path tops out
near 49k req/s, about a third of the fake's 139k, and at 200 closed-loop
workers that alone puts the gateway's mean latency near 4 ms.

**Streams** flush every chunk, as the design requires, so each chunk costs a
write syscall on the client connection; that dominates the stream profile.

## Open items

- A fair measurement of the 5 ms target needs the load generator and the
  fake on a different machine (or pinned cores) from the gateway, or an
  open-loop load at a fixed rate below saturation.
- Done after these measurements: the access log and the completion line,
  two lines per request and about 16% of the gateway's CPU here, are now
  one access-log line carrying the completion fields, as DESIGN §15.1
  describes. It did not measurably change the p99 (see above).
