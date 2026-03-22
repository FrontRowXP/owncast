# Incident Report: Livestream Outage — Root Cause Analysis

## 1. Incident Timeline

| Time | Event |
|------|-------|
| T-0 | Creator is actively live-streaming via RTMP to an Owncast pod |
| T+? | Owncast process crashes (see Root Cause below) |
| T+? | Kubernetes detects pod is unhealthy, restarts it |
| T+?+restart | On startup, `fixUnfinishedStreams()` marks the active stream as ended in the `streams` table |
| T+?+restart | In-memory state starts fresh: `_stats.StreamConnected = false` |
| T+?+restart | Status API (`/api/status`) reports `Online: false` |
| T+?+restart | **No `STREAM_STOPPED` webhook is ever sent** — the process was killed, not gracefully stopped |
| T+? | External manager polls status or never receives webhook → removes/does not repopulate the livestream servers table entry |
| T+? | Creator's encoder (OBS) loses the RTMP connection due to pod restart |
| T+? | On-call team discovers: livestream servers table is empty, manually re-adds entries |

## 2. Root Cause

**The Owncast process crashed during a live stream due to one or more `log.Fatal`/`log.Panic` calls in the video pipeline's hot path.** When the process crashes:

1. **No `STREAM_STOPPED` webhook is sent** — the `SetStreamAsDisconnected()` function never executes
2. **No graceful shutdown handler exists** — no signal handling (SIGTERM/SIGINT) anywhere in the codebase
3. **On restart, `fixUnfinishedStreams()` silently marks all active streams as ended**
4. **The external manager loses sync** — it either never receives the stop webhook, or it polls and sees offline status, but has no mechanism to distinguish "crashed and needs recovery" from "intentionally stopped"

### Primary Crash Vector: Nil Pointer Dereference in `saveOfflineClipToDisk`

**File:** `core/offlineState.go`, lines 96–110

```go
func saveOfflineClipToDisk(offlineFilename string) (string, error) {
    offlineFileData := static.GetOfflineSegment()
    offlineTmpFile, err := os.CreateTemp(config.TempDir, offlineFilename)
    if err != nil {
        log.Errorln("unable to create temp file for offline video segment", err)
        // BUG: err is logged but NOT returned. offlineTmpFile is nil.
    }

    if _, err = offlineTmpFile.Write(offlineFileData); err != nil {
        // If offlineTmpFile is nil (CreateTemp failed), this is a nil pointer dereference → PANIC
        return "", fmt.Errorf("unable to write offline segment to disk: %s", err)
    }
    // ...
}
```

If `os.CreateTemp` fails (temp dir full, wrong permissions, disk pressure on the pod), `offlineTmpFile` is `nil` and the subsequent `.Write()` call panics. This function is called from `SetStreamAsDisconnected()` which runs inside a goroutine (via `TranscoderCompleted`). An unrecovered panic in a goroutine **terminates the entire process**.

### Secondary Crash Vectors: `log.Fatal`/`log.Panic` in Video Hot Path

There are **18+ `log.Fatal`/`log.Panic` calls in the video processing pipeline** that will kill the process during a live stream:

| File | Line | Trigger Condition |
|------|------|-------------------|
| `core/streamState.go` | 64 | S3/storage setup fails when stream connects |
| `core/storageproviders/local.go` | 39, 54, 66 | Empty `streamID` during segment/playlist handling |
| `core/storageproviders/rewriteLocalPlaylist.go` | 19, 43 | Playlist file cannot be opened (disk/race) |
| `core/storageproviders/rewriteLocalPlaylist.go` | 52 | Empty `streamID` during playlist rewrite |
| `core/transcoder/transcoder.go` | 135 | FFmpeg stderr pipe fails |
| `core/transcoder/transcoder.go` | 140 | FFmpeg fails to start (panic) |
| `core/transcoder/utils.go` | 106, 113 | Variant directory creation fails |
| `core/transcoder/fileWriterReceiverService.go` | 43, 52 | Internal HTTP listener fails |
| `core/core.go` | 115 | Offline clip save fails (calls saveOfflineClipToDisk) |
| `replays/hlsRecorder.go` | 54, 73 | DB insert fails for stream/output config (panic) |
| `core/storageproviders/s3Storage.go` | 273, 286 | AWS credential/session setup fails (panic) |

### Why the Manager Did Not Recover

1. **No `STREAM_STOPPED` webhook**: Process crash = no cleanup = no webhook sent
2. **No graceful shutdown**: No SIGTERM handler → Kubernetes kill → same result
3. **Status API shows offline after restart**: Manager sees offline but has no way to know this was an unintentional crash (vs. a normal stream end)
4. **Creator's RTMP disconnected**: Pod restart severs TCP → OBS/encoder disconnects → no automatic reconnect by the creator
5. **No crash recovery webhook**: There is no "I just restarted from a crash" notification mechanism

## 3. Evidence

### Evidence 1: Nil pointer dereference in `saveOfflineClipToDisk`

```go
// core/offlineState.go:96-110
func saveOfflineClipToDisk(offlineFilename string) (string, error) {
    offlineFileData := static.GetOfflineSegment()
    offlineTmpFile, err := os.CreateTemp(config.TempDir, offlineFilename)
    if err != nil {
        log.Errorln("unable to create temp file for offline video segment", err)
        // BUG: Missing `return "", err` — continues with nil offlineTmpFile
    }
    if _, err = offlineTmpFile.Write(offlineFileData); err != nil {  // PANIC if offlineTmpFile is nil
```

### Evidence 2: `SetStreamAsDisconnected` early return skips webhook

```go
// core/streamState.go:82-130
func SetStreamAsDisconnected() {
    // ...
    offlineFilePath, err := saveOfflineClipToDisk(offlineFilename)
    if err != nil {
        log.Errorln(err)
        return  // EARLY RETURN: skips handler.StreamEnded(), skips webhook!
    }
    // ... handler.StreamEnded() never called
    // ... webhooks.SendStreamStatusEvent(models.StreamStopped, ...) never called
}
```

### Evidence 3: No signal/graceful shutdown handler

```bash
# Search for signal handling in the entire codebase:
grep -r "signal\|SIGTERM\|SIGINT\|graceful\|shutdown" --include="*.go" .
# Result: No matches found
```

### Evidence 4: `fixUnfinishedStreams` marks ALL active streams as ended on restart

```sql
-- db/query.sql:167-168
UPDATE streams SET end_time = (SELECT timestamp FROM video_segments
    WHERE stream_id = streams.id) WHERE end_time IS NULL;
```

This runs on **every startup** via `replays.Setup()` → `fixUnfinishedStreams()`. It marks **all** streams with `end_time IS NULL` as ended — including streams that were actively live when the pod crashed.

Additionally, the subquery `(SELECT timestamp FROM video_segments WHERE stream_id = streams.id)` returns an arbitrary row (no `ORDER BY` or `LIMIT 1`), so the `end_time` may not even be the last segment's timestamp.

### Evidence 5: Replay features are enabled, activating panic-prone code paths

```go
// config/config.go:47
var EnableReplayFeatures = true
```

```dockerfile
# Dockerfile:36
ENTRYPOINT ["/app/owncast", "-enableReplayFeatures", "-enableVerboseLogging"]
```

With replay features enabled, `replays.NewRecording()` is called on every stream start. This function calls `log.Panicln(err)` on DB insert failure — yet another crash vector.

### Evidence 6: Recent code changes introduced additional bugs (Dec 8–9)

**Commit `0f833b7c9`** — Commented out `#EXT-X-ENDLIST` from offline playlist, causing HLS clients to keep polling for segments after a stream ends.

**Commit `18a4c7b35`** — Multiple changes:
- Commented out `createEmptyOfflinePlaylist` in `makeVariantIndexOffline`, meaning if the playlist file doesn't exist during disconnect, no offline playlist is created and `_storage.Save` tries to upload a non-existent file
- Commented out `delete(s.queuedPlaylistUpdates, localFilePath)` in S3 storage, causing queued playlist uploads to never be cleaned up (unbounded re-uploads on every variant playlist write)
- Changed S3 file retention behavior for `stream.m3u8`

**Commit `12d0566fe`** — Changed offline segment duration from 15s to 1s.

### Evidence 7: Self-copy bug in `makeVariantIndexOffline`

```go
// core/offlineState.go:56
if err := utils.Copy(offlineFilePath, offlineFilePath); err != nil {
```

This copies a file to itself (source and destination are the same path). The `segmentFilePath` variable was commented out on line 53, leaving this as dead/incorrect code.

## 4. Contributing Factors

1. **Architecture**: Single-process design with no supervisor or sidecar health agent. Process death = complete loss of stream state.

2. **`log.Fatal` as error handling**: 18+ fatal/panic calls in the video pipeline turn recoverable errors into process-terminating events.

3. **No crash recovery mechanism**: No webhook or signal is sent when the process restarts after a crash. The external manager has no way to distinguish a crash-restart from a clean start.

4. **Replay features add panic paths**: With `EnableReplayFeatures = true`, every stream start goes through `InsertStream` which panics on any DB error.

5. **No idempotent recovery loop**: The manager apparently does not periodically reconcile the livestream servers table against actual Owncast pod status. It relies on point-in-time webhooks, which are lost on crash.

6. **Recent rapid code changes**: The Dec 8–9 commits modified critical offline state and S3 storage code paths with commented-out functionality rather than proper conditional logic, introducing silent failures.

## 5. Exact Failing Service / Code Path / Infra Component

### Failing Service
**Owncast** (the single Go process handling RTMP ingest, transcoding, HLS output, and status API)

### Failing Code Path (Primary)
```
TranscoderCompleted callback (goroutine)
  → SetStreamAsDisconnected()
    → saveOfflineClipToDisk("offline.ts")
      → os.CreateTemp fails (disk pressure / temp dir issue)
      → BUG: error not returned, offlineTmpFile is nil
      → offlineTmpFile.Write() → nil pointer dereference → PANIC
      → Unrecovered panic in goroutine → process termination
```

### Failing Code Path (Alternative/Secondary)
```
Any of the 18+ log.Fatal/log.Panic calls in:
  core/streamState.go:64
  core/storageproviders/local.go:39,54,66
  core/storageproviders/rewriteLocalPlaylist.go:19,43,52
  core/transcoder/transcoder.go:135,140
  replays/hlsRecorder.go:54,73
→ Process termination during live stream
→ No STREAM_STOPPED webhook
→ External manager loses sync
```

### Infra Component
- **Kubernetes pod** running the Owncast container — no health check that validates stream state, no graceful shutdown, no sidecar for crash detection
- **External manager service** — no reconciliation loop, relies solely on webhooks and/or status polling

## 6. Fix

### Immediate Fixes (applied in this branch)

1. **Fix `saveOfflineClipToDisk` nil pointer bug** — Return error immediately when `os.CreateTemp` fails
2. **Ensure `STREAM_STOPPED` webhook is always sent** — Move webhook call before the offline clip logic in `SetStreamAsDisconnected`, use `defer` pattern
3. **Replace `log.Fatal`/`log.Panic` in video hot path with proper error handling** — Return errors instead of killing the process
4. **Fix `fixUnfinishedStreams` SQL** — Add `ORDER BY timestamp DESC LIMIT 1` to the subquery
5. **Fix `queuedPlaylistUpdates` memory leak** — Uncomment the `delete` call
6. **Fix self-copy bug** — Correct the copy destination in `makeVariantIndexOffline`

### Code Changes Applied

See the commits on this branch for the specific code changes.

## 7. Preventive Actions

### Short-term
- [ ] **Add graceful shutdown handler**: Catch SIGTERM/SIGINT, call `SetStreamAsDisconnected()` and send `STREAM_STOPPED` webhook before exit
- [ ] **Add crash-recovery webhook**: On startup, if `fixUnfinishedStreams()` finds and fixes any streams, send a notification to the manager
- [ ] **Audit all `log.Fatal`/`log.Panic` calls**: Replace with error returns in any code path reachable during a live stream

### Medium-term
- [ ] **Add Kubernetes liveness/readiness probes**: The Dockerfile exposes ports but defines no `HEALTHCHECK`. Add health endpoint checks.
- [ ] **Add reconciliation loop in the manager**: Periodically sync livestream servers table against actual Owncast pod status, don't rely solely on webhooks
- [ ] **Add panic recovery in goroutines**: Wrap goroutines with `defer func() { if r := recover(); r != nil { ... } }()` to prevent unrecovered panics from killing the process
- [ ] **Add structured logging and alerting**: Log process crashes, unexpected restarts, and missed webhooks to a monitoring system

### Long-term
- [ ] **Separate stream state from process lifetime**: Use an external store (Redis, database) for stream state so it survives process restarts
- [ ] **Implement supervisor pattern**: Use a sidecar or init container that monitors the Owncast process and sends notifications on unexpected termination
- [ ] **Add integration tests for crash recovery**: Simulate process crashes during live streams and verify the manager correctly recovers
