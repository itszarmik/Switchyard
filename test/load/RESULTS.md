# Overhead benchmark results

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
