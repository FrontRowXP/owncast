# Incident Report: Livestream Production Outage — Mar 21 Root Cause Analysis

## 1. Incident Timeline

| Time | Event |
|------|-------|
| Mar 18 | **PR #5173 merged**: `channel-api` HPA scaled to `maxReplicas=40`, with comment "45 pods × 20 connections/pod = safe" |
| Mar 18–21 | System operates under normal load; connection headroom masks the miscalculation |
| T-0 (Mar 21, peak) | Traffic spike triggers `channel-api` HPA scale-up toward 40 replicas |
| T+? | Pod count crosses the threshold where total DB connections exceed the RDS limit of 5,000 (40 pods × 150 conn/pod = 6,000 possible) |
| T+? | **RDS connection exhaustion**: new connection attempts fail, existing queries start timing out |
| T+? | **Retry storms begin**: DB timeouts in `channel-api` and other services trigger retries, amplifying connection pressure |
| T+? | **NATS saturation**: retry storms elevate NATS subscription creation; NATS memory climbs toward the 4 Gi container limit |
| T+? | **NATS KV race condition**: 6+ pods performing non-CAS writes to `channel-connections` list silently overwrite each other under contention |
| T+? | Livestream services lose DB connectivity → Owncast pods cannot persist or read stream state → livestream goes down |
| T+? | Livestream servers table is empty — no active livestreams visible |
| T+? | External manager cannot recover because the underlying DB connection pool is exhausted |
| T+? | On-call team discovers the outage, manually re-adds livestream entries |
| Mar 21 | **PR #5236 opened**: root cause identified, HPA limits corrected, alarms tightened, NATS race fixed |

## 2. Root Cause

### Primary: Infrastructure Scaling Miscalculation (PR #5173)

**PR #5173** (merged Mar 18) scaled the `channel-api` HPA to `maxReplicas=40`. The PR comment stated "45 pods × 20 connections/pod = safe," but the **20 connections/pod figure came from the dev config**, not production.

**Production pods carry 150 connections each** (4 pgxpools × `entPoolMaxConns()` = 50 per pool × 4 = 200, with effective usage around 150). At peak:

```
40 pods × 150 connections/pod = 6,000 connections
RDS max_connections limit     = 5,000
Overshoot                     = 1,000 connections (20% over limit)
```

When the HPA scaled up during peak traffic, the total connection count crossed the RDS limit, causing **connection exhaustion** — new connections were refused and existing queries began timing out.

### Secondary: NATS Saturation from Retry Storms

DB timeouts in `channel-api` and other services triggered application-level retries. These retry storms amplified the problem:

1. Failed DB queries retried → more connection attempts → more failures
2. Retry storms elevated NATS subscription creation rate
3. NATS memory climbed toward the 4 Gi container limit
4. NATS throughput degraded, affecting inter-service communication

### Contributing: Legacy HPA Limits Across Multiple Services

Several other services had `maxReplicas=150` left over from a monolith split that was never revisited:

- `event`
- `socket`
- `cdnlogprocessor`
- `analytic-message-consumer`
- `message-consumer`

These services compounded the total possible DB connection count well beyond the RDS limit.

### Contributing: NATS KV Race Condition

A race condition existed in `RegisterConnection` / `RemoveConnection` — 6 pods were doing **non-CAS (Compare-And-Swap) writes** to the `channel-connections` list in NATS KV. Under contention, pods silently overwrote each other's writes, causing connection tracking to become inconsistent.

## 3. Evidence

### Evidence 1: Connection Math from PR #5173

PR #5173 comment: *"45 pods × 20 connections/pod = safe"*

The 20 conn/pod figure is from the **dev config**. Production config:
- `createPgxPool` default `MaxConns` = 350 (legacy footgun default)
- Effective per-pod: 4 pgxpools × `entPoolMaxConns()` = 50 → **~150–200 connections/pod**
- 40 pods × 150 = **6,000 connections** vs RDS limit of **5,000**

### Evidence 2: RDS Connection Limit

AWS RDS `max_connections` for the production instance = **5,000**. When `channel-api` scaled to 30+ pods, the connection pool exceeded this limit, causing `connection refused` and query timeout errors across all services sharing the database.

### Evidence 3: NATS Memory Spike

NATS memory approached the 4 Gi container limit during the incident window, driven by subscription creation from retry storms. No alert existed for this — the NATS memory alarm was only added in PR #5236.

### Evidence 4: NATS KV Non-CAS Writes

`RegisterConnection` and `RemoveConnection` in the connection tracking code performed plain writes to a shared NATS KV key (`channel-connections`). With 6+ pods writing concurrently, last-write-wins semantics caused connection lists to be silently corrupted — pods overwriting each other's entries.

### Evidence 5: Legacy maxReplicas=150

Multiple services still had `maxReplicas=150` from the monolith era. Combined worst-case pod counts for all services could request far more connections than the RDS instance supports.

### Evidence 6: RDS Alarm Detection Lag

The existing RDS CloudWatch alarm had a 10-minute detection window. A fast scaling spike could exhaust connections well before the alarm fired.

## 4. Contributing Factors

1. **Dev/prod config mismatch**: The connection-per-pod estimate used dev values (20) instead of prod values (150), a 7.5× undercount.

2. **No per-service connection ceiling**: There is no PgBouncer or RDS Proxy enforcing a hard connection limit per service. Any service can scale up and consume the entire RDS connection budget.

3. **Legacy HPA limits never revisited**: The monolith-to-microservices split left `maxReplicas=150` on services that should have been right-sized.

4. **No NATS memory alarm**: NATS could approach OOM without any alert firing.

5. **RDS alarm too slow**: The 10-minute detection window couldn't catch fast scaling spikes.

6. **Non-atomic NATS KV writes**: Connection tracking used plain writes instead of CAS operations, creating a race condition under concurrent pod access.

### Owncast-Level Code Issues (Additional Findings)

While investigating the Owncast codebase, several code-level bugs were also identified. These are **not the primary cause of this incident** (the infrastructure scaling issue is), but they represent latent risks that could cause or worsen future outages:

| Finding | File | Risk |
|---------|------|------|
| Nil pointer dereference in `saveOfflineClipToDisk` — `os.CreateTemp` error logged but not returned, causing nil pointer panic | `core/offlineState.go` | Process crash on temp dir failure |
| 18+ `log.Fatal`/`log.Panic` calls in the video pipeline hot path | Multiple files | Any recoverable error kills the process |
| `SetStreamAsDisconnected` early return skips `STREAM_STOPPED` webhook | `core/streamState.go` | External manager not notified of stream end |
| No graceful shutdown handler (SIGTERM/SIGINT) | `main.go` | Kubernetes pod termination sends no cleanup webhook |
| `FixUnfinishedStreams` SQL subquery has no `ORDER BY`/`LIMIT` | `db/query.sql` | Arbitrary segment timestamp used for end_time |
| `queuedPlaylistUpdates` map entries never deleted after successful upload | `core/storageproviders/s3Storage.go` | Unbounded re-uploads on every playlist write |
| Self-copy bug: `utils.Copy(offlineFilePath, offlineFilePath)` | `core/offlineState.go` | Offline segment never placed in correct location |

## 5. Exact Failing Service / Code Path / Infra Component

### Primary Failure Chain

```
PR #5173: channel-api HPA maxReplicas=40
    │
    ▼
Peak traffic → HPA scales channel-api to 30-40 pods
    │
    ▼
40 pods × 150 connections/pod = 6,000 DB connections
    │
    ▼
RDS max_connections = 5,000 → CONNECTION EXHAUSTION
    │
    ├──► DB queries timeout across all services
    ├──► Application retry storms amplify load
    ├──► NATS subscription creation spikes → memory saturation
    └──► NATS KV race condition corrupts connection tracking
            │
            ▼
    Livestream services lose DB access
            │
            ▼
    Livestream servers table appears empty
            │
            ▼
    External manager cannot read/write stream state → no auto-recovery
```

### Failing Components

| Component | Failure Mode |
|-----------|-------------|
| **channel-api HPA** (PR #5173) | Scaled to 40 pods, exceeding safe connection budget |
| **RDS (PostgreSQL)** | Connection limit exhausted at 5,000 |
| **NATS** | Memory saturation from retry-driven subscription storms |
| **NATS KV** | Race condition in `RegisterConnection`/`RemoveConnection` |
| **Livestream services** | Lost DB connectivity → couldn't persist/read stream state |
| **RDS CloudWatch alarm** | 10-min window too slow to catch fast scaling spikes |

## 6. Fix (PR #5236)

PR #5236 addresses the root cause and secondary effects:

| Fix | Detail |
|-----|--------|
| **HPA maxReplicas reduced** | Across 6 services so worst-case total stays under 5,000 DB connections |
| **RDS CloudWatch alarm tightened** | Detection window reduced from 10 min → 3 min to catch fast scaling spikes |
| **NATS memory alert added** | Alarm at 3.5 Gi (87.5% of the 4 Gi container limit) |
| **NATS KV race condition fixed** | `RegisterConnection`/`RemoveConnection` now use CAS (Compare-And-Swap) writes instead of plain writes |
| **`createPgxPool` default MaxConns reduced** | From 350 → 50 as a footgun prevention measure for future callers |

## 7. Preventive Actions

### Immediate (PR #5236)
- [x] Right-size HPA maxReplicas for all services based on **prod** connection counts
- [x] Tighten RDS alarm from 10 min → 3 min
- [x] Add NATS memory alert at 87.5% of container limit
- [x] Fix NATS KV race condition with CAS writes
- [x] Reduce `createPgxPool` default to prevent future connection budget overruns

### Short-term
- [ ] Add a **connection budget dashboard** showing per-service DB connection usage vs. RDS limit
- [ ] Add a **pre-merge CI check** that validates HPA maxReplicas × connections/pod ≤ RDS limit
- [ ] Audit all services for dev/prod config mismatches in connection pool settings
- [ ] Fix Owncast-level code bugs (nil pointer, log.Fatal in hot paths, webhook reliability — see Additional Findings above)
- [ ] Add graceful shutdown handler to Owncast for proper SIGTERM cleanup

### Long-term
- [ ] Deploy **PgBouncer or RDS Proxy** to enforce a hard per-service connection ceiling regardless of pod count
- [ ] Implement **connection-aware HPA scaling** that factors in DB connection budget, not just CPU/memory
- [ ] Add **circuit breaker** patterns to prevent retry storms from cascading across services
- [ ] Centralize HPA configuration review as part of the infrastructure change approval process
