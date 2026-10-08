# Measurement results

Log of every load test run so far, with what each one showed and what is still
unknown. The JSON report of each run is in this folder and can be checked against the
numbers below.

The results are **relative comparisons between configurations on the same machine**.
They are not production capacity.

The JSON reports behind sections 2, 3 and 6 were pruned from this folder on 2026-10-07,
once the Postgres config changed and those numbers stopped being the current baseline
(see "Postgres configuration" below). They are not lost: every file is in git history up
to commit `2b1c222`, retrievable with `git show 2b1c222:docs/bench/<filename>`.

## Environment

| Item | Value |
|---|---|
| CPU | 6 cores, 12 threads (hyperthreading) |
| Memory | ~15 GiB |
| OS | Windows 11, Docker Desktop with the WSL2 backend |
| PostgreSQL | 16.15, `postgres:16` container |
| Go | 1.26.0 (local toolchain) |
| API and generator | same machine, no CPU isolation |

## Postgres configuration

Up to and including section 6, every run used the stock `postgres:16` image config
(`shared_buffers` 128MB, `effective_cache_size` 4GB, `max_wal_size` 1GB,
`random_page_cost` 4 — all defaults, nothing set by this project).

Starting 2026-10-07, `docker-compose.yml` sets `shared_buffers=1GB`,
`effective_cache_size=10GB`, `max_wal_size=4GB`, `random_page_cost=1.1` on the
`postgres` service, sized for this host (12 threads, ~15 GiB, nothing else
competing for the container). `fsync` and `synchronous_commit` were left `on`:
these four changes give Postgres more of the headroom the host already had, they
do not change any durability guarantee. Any run after this line is **not**
comparable to sections 2 and 6 without saying so.

## General caveats

- **Host noise.** The generator, the API, Postgres and the Docker VM share the same
  12 threads. The CPU baseline varied from 1% to 69% between identical runs.
- **Pool size is the easiest variable to get wrong.** The API only uses a new value after
  it is **rebuilt** (`docker compose up -d --build app`). For a while, the runs labelled
  "30" actually ran with 10 connections, because the container had not been rebuilt.
  See section 2.
- **The JSON records the pool size only if the operator declares it** (`-api-max-conns`).
  It is not read from the API. When the declared label does not match reality, the report
  is wrong without any warning.
- **Different versions of the tool.** The preliminary measurements were taken before
  worker staggering, seeding and the JSON report existed. That is why they have no file
  in this folder.
- **The database grows.** Transfers accumulate rows. The database went from ~8 MB to
  ~268 MB over these tests, which can affect the `read` scenario.

## 1. Tool validation runs

These runs do not measure the system. They check that the tool behaves as documented.

| File | Test | Result |
|---|---|---|
| `hot-seed9-…json` | CPU limit of 1% | Stopped by itself in the first stage (8 workers) with `resource_limit`. Confirms the host guard triggers. |
| `hot-seed10-…json` | `-rate 200`, 4 workers | 193 req/s against a target of 200. Confirms per-worker pacing. |

## 2. Connection pool: curve from 10 to 50

Scenario `spread` (transfers between 100 accounts), fixed level of 32 workers, seed 42,
15 s stages. No run had an application or connection error.

The real connection count of each block comes from the **order of the runs and the
container rebuilds**, not from the JSON. This is the correct classification:

| Block | Files | Real connections | Label in JSON | n | Median req/s | Mean req/s | CPU baseline (mean) |
|---|---|---|---|---|---|---|---|
| A | `…173521` to `…174157` | 10 | no field | 9 | 2154 | 2179 | not recorded |
| B | `…174932` to `…175037` | 10 | **30 (wrong)** | 5 | 2477 | 2357 | ~6% |
| C | `…175150` to `…175256` | **30** | 30 | 5 | **3667** | 3569 | ~11% |
| D | `…175424` to `…175529` | 10 | **30 (wrong)** | 5 | 2529 | 2380 | ~6% |
| E | `…180048` to `…180153` | **20** | 20 | 5 | 3312 | 3341 | ~7% |
| F | `…180246` to `…180352` | **40** | 40 | 5 | 3321 | 3458 | ~7% |
| G | `…180447` to `…180553` | **50** | 50 | 5 | 3324 | 3432 | ~6% |

Blocks B and D had the `api_max_conns_declared` field set to 30, because the command-line
argument was not updated when `MaxConns` went back to 10. The reports are correct in every
other field. The JSON files were not edited, because they are measurement data. Blocks B
and D must be read as 10 connections.

**Curve (median, 5 runs per block):**

| Connections | Median req/s |
|---|---|
| 10 | ~2500 |
| 20 | 3312 |
| 30 | 3667 |
| 40 | 3321 |
| 50 | 3324 |

**Reading:**

- **From 10 to 20 connections, the gain holds**: from ~2500 to ~3300 req/s (+33%). The
  difference is large and appears in every block.
- **From 20 to 50 there is a plateau**, around 3300 to 3700 req/s. The peak of 30 (3667) is
  about 10% above the plateau, but that difference is smaller than the spread inside each
  block (16 to 30%). With the current data, it is not possible to say that 30 is better
  than 20, 40 or 50.
- **The CPU baseline does not explain the plateau.** The 20, 40 and 50 blocks had similar
  background CPU (5 to 7%), and throughput stayed at the same level.
- **Operational choice.** The `MaxConns` in the code is **30**, which had the highest
  median of the curve. The choice is not proof of optimality: any value between 20 and 50
  sits on the same plateau, within what these measurements can separate.

**Correction of earlier text.** The commit message `a7bb22a` says that `spread` went from
~1250 req/s with 10 connections to ~2100-2700 with 30. Those numbers are wrong. The ~1250
figure came from an earlier version of the tool, and the ~2100-2700 figure came from runs
with 10 connections. The correct comparison is the table above.

## 3. Scenario `hot` (all transfers on the same account)

| Connections | Report | Stages (workers: req/s, p99) | Stop |
|---|---|---|---|
| 10 | `…1791231999…` | 8: 514, 19 ms · 16: 385, 47 ms · 32: 326, 111 ms · 64: 319, 246 ms · 128: 309, 440 ms · 256: 285, 934 ms · 512: 258, 2030 ms | API (p99 > 2 s) at 512 |
| 10 (JSON label: 30) | `…1791232403…` | 8: 508, 19 ms · 16: 391, 47 ms · 32: 324, 110 ms · 64: 284, 268 ms · 128: 260, 526 ms · 256: 318, 835 ms · 512: 271, 2330 ms | API (p99 > 2 s) at 512 |

**Reading:** throughput stays around 300 req/s whatever the pool size. The bottleneck is the
lock on the account row, which serializes the transfers. More connections do not help
here, and p99 grows linearly with the workers, as predicted by Little's law.

Both `hot` reports ran with 10 connections in fact. The second one had the
`api_max_conns_declared` field recorded as 30, but the container had not been rebuilt.
Since this scenario is limited by the lock, the conclusion holds, but the label must be
read as 10.

## 4. Database ceiling with `pgbench`

Standard workload (TPC-B), scale 10, 30 s per run, 4 client threads, run inside the
Postgres container in a separate database (`pgbench_test`).

| Clients | Mean tps | Mean latency |
|---|---|---|
| 8 | 2687 | 3.0 ms |
| 16 | 3380 | 4.7 ms |
| 32 | 2567 | 12.5 ms |

**Reading and caveat:** `pgbench` measured about 2500 to 3400 tps on this machine, at a
different time and with a different tool. The API reached ~3300 to 3700 req/s with 20 to 50
connections, so **this number is not a reliable ceiling**, and it is not possible to claim
that the API is at the database limit. To use `pgbench` as a reference, it has to be
repeated under the same conditions (idle host, same time window) before any comparison.

## 5. Preliminary measurements (no JSON report)

Taken with the first version of the tool, before worker staggering. The numbers come from
the terminal output.

- **`hot`, 10 connections, 512 workers:** 192 req/s, with 5.5% `connection_refused`. This
  was the first sign that the failure was connection-level and not latency.
- **`spread`, 10 connections, 32 workers:** 1293 and 1216 req/s in two runs. These come from
  an older version of the tool and must not be compared directly with sections 2 and 4.
- **`read`, 10 connections:** about 6300 to 6900 req/s between 8 and 1024 workers, with
  `connection_refused` above 512.
- **`hot` with `-max 5000`, 30 s per stage:** 265 req/s at 64 workers, falling to 191 at
  512, when p99 went past 2 s.

## 6. Locking vs serializable (same load, alternating blocks)

Scenarios `hot` and `spread`, seed 42, 8 to 128 workers doubling, 15 s stages, `-api-max-conns 30`.
Each strategy ran 5 times per scenario, alternating `locking` and `serializable` in the same
order (hot first, then spread). The `.env` was changed between blocks and checked with
`docker inspect` before each run. Each run restarted the app afterwards, so the retry counter
in the shutdown log belongs to that run only. Container rebuilds were not needed, since only
the environment variable changed.

Command for each run (PowerShell, one line):

```powershell
go run ./cmd/loadtest -scenario hot -max 128 -stage 15s -seed 42 -accounts 100 -api-max-conns 30
```

The run used the scenario and strategy from the `.env` at the time. The `-strategy` flag is
not implemented yet, so the strategy is recorded only by the `.env` state and by the
run index, not in the JSON.

**Peak throughput (highest stage rps per run, 5 runs each):**

| Scenario | Strategy | Median | Range | Last stage reached | Stop reason |
|---|---|---|---|---|---|
| hot | locking | 350 req/s | 308 to 466 | 128 workers in all 5 runs | none (max workers) |
| hot | serializable | 341 req/s | 256 to 368 | 16 workers (2 runs), 32 workers (3 runs) | `api_broke` |
| spread | locking | 2548 req/s | 2314 to 2848 | 128 workers in all 5 runs | none (max workers) |
| spread | serializable | 741 req/s | 722 to 839 | 64 workers in all 5 runs | `api_broke` |

**What the runs show, without interpretation:**

- **Locking never returned a non-2xx status.** The 10 locking runs had 0 non-201 responses,
  and all 10 passed the ledger check.
- **Serializable returned `500` under contention.** The non-201 responses were all `500`,
  with no `503` and no `409`. They were 5.6% of requests on `hot` (824 of 14611 in run 1)
  and about 2.3% on `spread` (971 of 41399 in run 1). These are the requests that exhausted
  the retry limit, as the code is written. The ledger check still passed in every run, so
  the failed transfers were not applied. The code path is confirmed: after `maxAttempts`
  the executor wraps the `40001` error, and `statusFor` has no case for it, so it falls to
  the default `500 internal_error`.
- **Serializable broke the API on `spread` at 64 workers in all five runs**, with 500s as the
  cause. At 8 workers it already returned some `500` (9 of 10202 in run 1), before the break.
- **Retries per request** (retries counted by the app, divided by the requests of the run):
  1.05 to 1.73 on `hot`, 1.49 to 1.63 on `spread`.
- **Requests completed in the same time.** Over a full `spread` run, locking completed about
  154 000 to 173 000 requests, and serializable about 40 000 to 46 000, with the same stage
  durations.
- **Pool size during the run.** A `pg_stat_activity` sample at about 45 s showed 31 sessions
  in the locking runs (30 pool connections plus the sampler's `psql` session), which matches
  `MaxConns` 30. The serializable samples showed fewer sessions, but they were taken while
  the API was already breaking.
- **Comparison with the earlier pool curve (section 2).** The median for `spread` with 30
  connections was 3667 req/s there, and 2548 req/s here. The gap shows that the host varies
  between sessions. These numbers should only be compared inside this matrix.

**Decisions left to the author** (the analysis of why each strategy behaves this way is for the
author to write, as the project's working rules say):

- Whether the `500` under retry exhaustion is acceptable, or whether it should be a retryable
  status such as `503` or `409` (see the open item on `maxAttempts` below).
- Whether to raise `maxAttempts` (currently 10) and measure again.
- Whether the strategy should be recorded in the JSON (the `-strategy` flag), so that a future
  run does not depend on the `.env` state.

Matrix reports: `docs/bench/hot-seed42-20261006-2204*` to `…2225*` and
`docs/bench/spread-seed42-20261006-2206*` to `…2227*`, one JSON per run, listed by timestamp.
The index with retries and the database sample is kept outside the repository.

## 7. Re-measurement under the tuned Postgres config

Same two experiments as sections 2 and 6 (pool curve and locking vs serializable),
repeated after the `shared_buffers`/`effective_cache_size`/`max_wal_size`/
`random_page_cost` change above. `WALLET_DB_MAX_CONNS` (new, see below) made this
run without any container rebuild between blocks.

**Pool curve** (`spread`, fixed 32 workers, `locking`, seed 42, 15 s, 5 runs per size):

| Connections | Median req/s (tuned) | Median req/s (stock, section 2) |
|---|---|---|
| 10 | 2377 | ~2500 |
| 20 | 3016 | 3312 |
| 30 | 3427 | 3667 |
| 40 | 2972 | 3321 |
| 50 | 3232 | 3324 |

Every tuned-run value is below the corresponding stock-run value, by 5% to 10%. The
*shape* of the curve repeats (rises from 10 to 30, plateaus or dips from 30 to 50), but
the tuning did not raise throughput here — if anything this session's host was slightly
noisier. This matches the caveat already in this document: numbers drift between
sessions more than the difference any one setting makes. No run failed and no run lost
ledger consistency.

**Locking vs serializable** (same command as section 6, seed 42, 5 runs per scenario):

| Scenario | Strategy | Median req/s (tuned) | Median req/s (stock, section 6) |
|---|---|---|---|
| hot | locking | 318 | 350 |
| hot | serializable | 271 | 341 |
| spread | locking | 2705 | 2548 |
| spread | serializable | 682 | 741 |

Only `spread`/`locking` moved in the direction tuning would predict (+6%), and it is the
one scenario with little lock contention, where more `shared_buffers` and a wider
`max_wal_size` have the most room to help. Every other number went down slightly,
inside the same run-to-run noise seen in section 2. The retry counts stayed in the same
range as before tuning: `hot`/`serializable` 10 200 to 21 677, `spread`/`serializable`
66 400 to 69 600. Retries come from concurrent transactions overlapping, not from disk
I/O, so a storage-side tuning change was not expected to move that number, and it did
not. All 20 runs kept `500` as the only non-2xx status, and ledger consistency passed in
every run.

**Reading.** On this host, for this workload, the four tuning changes did not produce a
measurable improvement outside of normal session noise. The `hot` plateau and the
`serializable` retry volume are set by contention at the row/transaction level, not by
how much of the host's RAM Postgres was allowed to use. This does not rule out the
`fsync`/`synchronous_commit` hypothesis from the same discussion — that one was never
tested, since it trades away a durability guarantee and was treated as a separate,
disposable experiment, not a committed config change.

**New since section 6:** `WALLET_DB_MAX_CONNS` (env var, default 30) replaced the
hardcoded pool size in `cmd/wallet/main.go`, and the app now logs `transfer_strategy`
and `db_max_conns` at startup. Changing either no longer needs an image rebuild, only
`docker compose up -d app` — the exact mistake that produced the mislabeled blocks B and
D in section 2 is no longer possible this way.

## Still open

- **More repetitions of the 20 to 50 connection blocks**, to separate the values inside the
  plateau, which the current spread cannot tell apart (16 to 30% within each block).
- ~~Confirm the real connection count during a run with `pg_stat_activity`.~~ Done in
  sections 6 and 7: a sample was taken mid-run in every one of the 45 matrix runs. The
  locking samples land close to the declared pool size (for example 30 or 31 sessions at
  `-api-max-conns 30`, the extra one being the sampler's own connection).
- **Test the `synchronous_commit = off` hypothesis for the `hot` plateau** (see the
  concurrency discussion before section 7). Not run yet: it trades away a durability
  guarantee, so it should stay a disposable, undocumented-as-config experiment, not
  something committed to `docker-compose.yml`.
- **Repeat `pgbench`** under the same conditions as the API, to get a valid reference.
- **Repeat the `read` scenario** with the current database, since it has grown a lot.
- **Explain the connection failures** that appeared at 512 workers in the preliminary
  measurements. The hypothesis is the Windows `accept` queue, not yet confirmed.
- **Check the `503` from `AcquireTimeout` (3 s)**, which has not been observed yet. It only
  shows up with more workers than the pool can serve, and the resource limit must be
  disabled for that.
- **Fix the label in future runs**: always pass `-api-max-conns` equal to the API's real
  value, or pass nothing when there is no certainty.
