# Recording Subsystem Redesign

Status: proposal, 2026-10-22, against main `ccb54e1` (after #508). Steps 2
and 3 (section 7) are implemented in the follow-up PRs to this one. Scope:
`internal/recording` (recorders, spool, S3 upload, retention, storage alerts)
and its coupling to `internal/encoder`.

This document explains why the recording subsystem needs a redesign instead
of more fixes, states the guarantees it must give, proposes a structure that
gives them by construction, and defines how we prove the result. It comes with
an executable invariant corpus, `internal/recording/invariants_test.go`. The
corpus checks the guarantees against today's code and will check the redesign
the same way.

## 1. Why: the evidence

### 1.1 Fixes that follow fixes

Between 2026-06-12 and 2026-10-21 (UTC), 35 merged PRs changed files under
`internal/recording`, 32 of them production code. 23 of them landed between
16 and 21 October, the last being #508. Nearly every recording issue from #419
onward states it was found while reviewing an earlier PR. Review/follow-up chains, as
named in the issues:

```text
#408 -> #439, #441, #442, #443
#418 -> #434 -> #467, #473
#441 -> #477
#439 -> #479 (#457); #439/#440 -> #480 (#458, stacked on #479)
#443 -> #475
#442 -> #482 -> #506
#473 -> #507
```

Two policies flipped back and forth:

- **Partial file after a write error:** uploaded directly (#299), then put on
  the persisted retry queue (#439), then quarantined (#480).
- **Output overwrite policy:** `-y`, then `-n` with a stat check, then stderr
  matching, then an O_EXCL placeholder with `-y` and an inode check (#408,
  #439, #479).

After #508 closed #484 and #487, 12 issues about recorder, spool, upload and
storage behaviour are still open: #486, #488, #495, #496, #502, #511–#515,
#517 and #518. Two related issues are also open: #459, fixed but not closed,
and #499, about CI coverage.

### 1.2 Guards accumulate instead of converging

Hotspots since 2026-06-01, at `ccb54e1` (non-test files):

| File | Commits | Lines then → now |
|---|---|---|
| recorder.go | 28 | 699 → 1225 |
| manager.go | 17 | 323 → 516 |
| recorder_upload.go | 15 | 358 → 535 |
| spool.go | 5 | 0 → 303 |
| storage.go | 3 | 0 → 278 |

Since June, PRs touched these functions:

| Function | PRs |
|---|---|
| `startAsync` | 9 |
| `rotateFile` | 8 |
| `stopLocked` | 8 |
| `startEncoderLocked` | 7 |
| `finishEncoder` | 7 |

The package has 6,831 test lines in 28 files against 3,359 production lines
in 14 files, and every one of those test files is white-box (`package
recording`).

Run ownership alone is guarded by twelve separate mechanisms:

- `startGen`, `rotationGeneration`, `durationGeneration`, `restoreGen` and
  `sourceRunID`;
- `r.result != result`, `claimedGeneration`, `cleanupStopCh != stop` and the
  recheck loop on `r.config != cfg`;
- the `removing` set, `ErrRecorderStopping` and `lifecycleMu`.

#508 adds `reconcileMu` and a stop-done channel.

"Running" is spread over six fields: `state`, the `recording` atomic,
`result != nil`, `audioCh != nil`, `cancelUploads != nil` and `uploadClosed`.
Between `c33198c` (June) and `3e82d0c` the recorder struct grew from 23 to 38
fields, and its sync/atomic fields from 3 to 8.

Fifteen places start background work:
- 11 `go` or `WaitGroup.Go` sites;
- 2 `afterFunc` sites;
- 2 `RunHourly` loops.

`processRetryQueue` alone is reached from five launch chains plus the hourly
tick.

### 1.3 Three structural causes

1. **A recorder run has no single owner.** Seven continuations act on a run:
   the clock-sync wait, file preparation on a slow share, the rotation timer,
   the duration timer, a writer error, the upload worker and the retry pass.
   Most of them release the lock, do I/O, take the lock again and must then
   prove that they still own the run. The issues listed below are cases where
   a proof was missing, and the fixes for them added a generation counter or
   a flag.
   Issues: #434, #467, #473, #477, #484, #486, #487, #514 and #515.

2. **The upload queue lives in memory, and metadata is attached afterwards.**
   An undelivered recording can be tracked in six places:
   - the in-memory channel, with no upload metadata yet;
   - the worker's in-flight upload, whose sidecar is written only when the
     first attempt starts;
   - `retryQueue`;
   - the path set `retryInFlight`;
   - the sidecars of a removed recorder;
   - a failed output with its `.unfinalized` marker.

   When upload metadata is persisted depends on the path. Sidecars reload in
   every mode, but only S3-only mode rediscovers a recording that has no
   sidecar. Each counter looks at a different subset of these places.
   Issues: #488, #496, #502, #517 and #518, and earlier #442, #443 and #475.

3. **Lifecycles are coupled that should not be.**
   - Recorder stop does finalization, upload handoff and retry scheduling.
   - The recording manager stops and starts with the encoder's lifecycle. An
     audio input change (`api.go:238` → `RestartInBackground` → `stop` →
     `stopConsumers`, `encoder.go:591`) stops every recorder.
   - Retry passes run on `context.Background()` and nothing joins them, so
     process exit cuts them off (#518).
   - Status, `/ready` and alerts each derive health from different sources
     (#488, #502, #511, #514).

## 2. Requirements

Decisions taken on 2026-10-22:

- **R1. Recordings survive encoder restarts.** Changing the audio input or
  restarting the source does not stop, cut or restart recorders. A recording
  is cut only by:
  - its own stop;
  - an hourly rotation;
  - the process ending;
  - an operator saving new output settings (D3).

  A *lost* input already keeps recorders running (`encoder.go:876`).
- **R2. Local+S3 mode gives the same S3 guarantee as S3-only mode.** Every
  recording reaches S3, also after a crash and after its recorder is removed.
- **R3. Uploads are retried forever.** Retries use backoff and raise an
  overdue alert. Nothing is abandoned or deleted before delivery unless an
  operator explicitly discards it. Disk pressure is alerted and is the only
  limit.
- **R4. No on-disk migration.** Existing spool directories and upload sidecars
  need not be readable. The external interfaces stay: local filenames, S3 keys
  (`recordings/<prefix>/<file>`) and the HTTP API. Event types may gain
  members; `upload_abandoned` disappears.
- **R5. Platforms.** Raspberry Pi on an SD card, where power loss is real and
  fsync is slow, and Windows PCs.

Open PRs were merged first; #508 landed on 2026-10-21 at 22:27 UTC.

## 3. Invariants

These are the contract. Each one names its corpus scenario; "gap" means the
scenario needs a seam the redesign introduces (section 6).

| # | Invariant | Corpus scenario |
|---|---|---|
| I1 | Capture never waits on recording. | existing white-box tests; keep |
| I2 | Every chunk accepted while running ends up in a finalized file, or is counted in `AudioDrops`. | `RoundTrip/*` (normal stop only) |
| I3 | After a crash of the service, with or without its FFmpeg children, restart finds every recording that holds audio, in every storage mode. After power loss, audio up to the durability watermark survives (section 4.3). | `Durable/*` (service and tree crash at one point; gap: power loss, more crash points) |
| I4 | Each recording reaches each destination exactly once and never replaces an object it did not write. | `RoundTrip/*`, `ExactlyOnce/*`, `Durable/*` |
| I5 | Stopping a recorder or the manager is bounded by local work (FFmpeg finalization), never by the network. | `BoundedStop/*` |
| I6 | Every undelivered recording is counted, including during an upload and after its recorder is removed. All counts derive from one source. | `HonestPending/*` |
| I7 | After `Manager.Stop` no recorder runs. The saved configuration is what runs. | `ClosedLifecycle/*`, `ConfigApplies/*` |
| I8 | A failure stays visible until a later success or an operator action clears it. | `ErrorStaysVisible/*` |
| I9 | Nothing deletes an undelivered recording except the operator. | gap: needs a clock seam |
| I10 | An undelivered recording is retried until delivered, and is alerted as overdue after a threshold. | gap: needs a clock seam |
| I11 | An input change or source restart does not cut a recording (R1). | gap: encoder-level scenario |
| I12 | During an input outage the file keeps pace with wall-clock time (silence), and a segment with only silence is discarded (D1). | gap: needs a clock seam |

About I9: today, in local+S3 mode, retention (`cleanup.go:92–106`) protects
only the current and retiring files and files modified within the retention
period. An older file whose upload is still pending is deleted. The retry
pass then drops the upload with only a warning ("retry file no longer
exists", `recorder_upload.go:480`). Depending on retention, uptime and
restarts, the 24-hour abandon alert may fire first. Under R3 there is no abandon alert, so retention must consult the
outbox.

## 4. Proposed design

```text
                 capture (distributor)
                        │ WriteAudioShared (atomic recorder view,
                        │ no management locks)
                        ▼
 ┌──────────── Manager (process lifetime, one ctx, Close joins) ┐
 │                                                              │
 │  Recorder (one goroutine owns the run)                       │
 │    commands ─▶ loop ─▶ helpers: reserve, start FFmpeg,       │
 │    timers   ─▶  │       finalize, fsync (results as events)  │
 │                 │ publishes Status snapshot (atomic)         │
 │                 ▼                                            │
 │  Outbox (on disk: the queue)  ◀── Retention (skips tickets)  │
 │    ticket per recording: recording → ready → delivered → ∅   │
 │                          └→ failed (until operator acts)     │
 │                 │                                            │
 │                 ▼                                            │
 │  Uploader (one per manager: backoff, overdue, joined)        │
 │                 │                                            │
 └─────────────────┼────────────────────────────────────────────┘
                   ▼
                  S3
```

### 4.1 Recorder: one goroutine owns the run

- **One loop goroutine per recorder.** Only that goroutine reads or writes the
  run state: current segment, timers, config and error. Everything else
  talks to it through commands (start, stop, update, activate, deactivate,
  close), each run in the loop while the caller waits. After `Manager.Stop`
  the loop rejects a start until the next `Manager.Start`.
- **Blocking work never runs in the loop.** Reserving the output on a slow
  share, starting FFmpeg, periodic fsync, and draining and finalizing all run
  in helpers that send their result back as an event. A slow disk therefore
  never blocks `Status` or a queued command (#477, #486).
  - `Stop` still waits for finalization, which is bounded by FFmpeg's
    drain/kill escalation.
  - A helper whose result arrives for a segment that is no longer current
    cleans up what it created (discards the reservation, kills the process)
    instead of being silently dropped.
  - Recorder events are written in order by a writer outside the loop.
  - Removing a recorder, or `Manager.Close`, waits up to 10 seconds for its
    stop, helpers and events (#526). A helper still hung on a share after
    that starts no FFmpeg and discards its reservation.
- **Timers run in the loop.** A timer acts only while it is still the one
  the loop armed, so stopping or replacing it also cancels a callback that
  already fired. The hourly rotation and the retry of a failed hourly run are
  such timers, and so is the on-demand duration limit, which belongs to the
  run and keeps its deadline across segment changes. That check replaces
  `startGen`, `rotationGeneration`, `durationGeneration`, `claimedGeneration`,
  the recheck loop and the manager's hourly retry scheduler.
- **A segment** is prepared (the only one whose result the loop takes),
  current, then finalizing until it is committed, failed or empty. Committed
  means the outbox holds the recording.
- **`Status()` reads an `atomic.Pointer[Status]`** published by the loop, so
  it takes no recorder lock; the manager holds `m.mu` only to read its map.
- **Config rules:**
  - `enabled` and the recording mode apply immediately (#487; #508 already
    does this).
  - Saving new output settings (codec, bitrate, path, storage mode, S3
    endpoint or bucket) starts a new segment immediately with a
    make-before-break rotation (#512, D3). "Changes apply on save" then
    holds.
- **The capture path stays as it is:** a bounded per-segment channel feeds a
  writer goroutine for FFmpeg stdin, and rotation is make-before-break (#299,
  #418, #441).

### 4.2 Outbox: the disk is the queue

- **One ticket per recording** in `<spool>/outbox/`, a flat directory; the
  ticket names its recorder.
  - S3-only recordings are spooled under `<spool>/recordings/<prefix>/`,
    with the prefix from `RecordingFilePrefix()` instead of the raw ID
    (#495).
  - A ticket holds the recorder ID, the local path, the destination
    (endpoint, bucket, base key and a destination ID), the upload ID, the
    state, the attempt count, when it became ready, the next attempt and the
    last error.
  - Credentials live once per destination in
    `<spool>/outbox/destinations/<id>.json` (0600, the same trust boundary as
    `config.json`).
  - A ticket keeps its destination. Editing a recorder's bucket affects new
    recordings only, which deliberately changes today's retarget-on-edit
    behaviour. An operator can retarget outstanding tickets explicitly.
- **The ticket is written when the output is reserved,** before FFmpeg gets
  audio, in every storage mode, together with an fsync of the output's
  directory entry. It replaces the `.unfinalized` marker and its
  permission-bit encoding.
- **State machine:**
  - `recording` → `ready` at finalization (S3 modes). In local-only mode the
    ticket is removed at finalization instead, because nothing is left to
    deliver.
  - `ready` → `delivered` once S3 confirms the object.
  - `delivered` → removal of the spool copy (S3-only mode) → removal of the
    ticket.
  - Any state → `failed` when finalization fails. A `failed` ticket stays
    until the operator deletes its file; the next start then removes the
    ticket. A `lost` ticket is cleared the same way.
- **Recovery** (one scan at startup) depends on the storage mode:
  - `recording` with audio → `ready` in the S3 modes, and simply removed in
    local-only mode, where the file stays as it is. Either way an
    `interrupted` event is logged. This also covers the interrupted local
    file in local+S3 mode, which today is never uploaded.
  - `recording` without audio, never synced (section 4.3) → removed. Nothing
    durable was promised for it.
  - `recording` whose file is missing or empty although the ticket records a
    completed sync → `lost`, with an alert.
  - `ready` with the local file missing → the uploader HEADs the key. If
    the object carries the ticket's identity, delivery is finished. If there
    is no such object, the ticket becomes lost, with an alert.
  - `delivered` with the local file gone → only the ticket is removed; S3
    already confirmed the object.

  The scan builds an in-memory index that afterwards changes only together
  with a ticket write, or in memory alone when a write fails, so a backoff
  still holds. A ticket the scan cannot read is left on disk, counted as
  lost, and pauses retention and destination pruning until a restart reads
  it. Pending, overdue, failed and lost counts all come from
  that index (I6). The in-memory channel, `retryQueue`, `retryInFlight`, the
  abandoned map and its rescans disappear.
- **Removed recorders.** Their tickets keep their destination and are still
  delivered. They are counted under the removed recorder until delivered or
  discarded (#502, R2).
- **Failed tickets** (quarantined recordings) are counted and alerted, and
  never expire on their own (#496, I9). The operator handles one by deleting
  its file and restarting the service.

### 4.3 Durability boundary

A ticket makes a recording *discoverable*, not its audio *durable*. The two
failure classes have different guarantees.

- **Service crash** (kill, panic, OOM). The kernel keeps every byte that
  FFmpeg had written.
  - FFmpeg may survive and finalize at stdin EOF. Under `KillMode=mixed`,
    systemd can also SIGKILL it together with the service.
  - The file therefore ends at FFmpeg's last write, not at the moment of the
    crash. What is lost: audio still in the capture buffer (at most about
    3 s), audio in FFmpeg's encoder and muxer, and an unwritten trailer.
  - FFmpeg runs with `-flush_packets 1`, so finished packets reach the kernel
    as they are muxed. That does not bound the encoder's own backlog.
  - MP3 and MPEG-TS stay playable up to the last complete frame.
- **Power loss.** The page cache is lost.
  - While recording, a helper fsyncs the output every W = 30 seconds (D4).
    On Linux, fsync on any descriptor of the file flushes its
    data. On Windows, `FlushFileBuffers` on a handle with write access flushes
    data and metadata.
  - The guarantee is a **durability watermark**: the output length taken just
    before a sync starts, recorded once that sync *completes*. Each sync that
    finds the file grown writes it to the ticket as a new generation.
  - The bytes up to the watermark, and the audio they encode, survive a power
    cut. Later bytes may be lost.
  - Capture and encoder buffering mean the watermark trails real time by more
    than W.
  - A sync that fails raises a recorder warning, and the watermark does not
    advance.
- **Finalization order.** First FFmpeg exits, then the recorder fsyncs and
  closes the output, and only then is the ticket written as `ready` (or
  removed in local-only mode). A recording is never marked deliverable before
  its bytes are durable.
- **Ticket updates do not rely on atomic replace.**
  - Each state change writes a new file `<ticket>.<seq>.json` and flushes it.
    On Linux the directory is then synced. After that the older generations
    are removed.
  - Recovery takes the highest sequence number that parses.
  - On Windows, Go's `os.Rename` calls `MoveFileEx` without
    `MOVEFILE_WRITE_THROUGH` and gives no atomicity promise, so the design
    does not rely on it.
  - Windows has no directory fsync. A new generation file is created and
    flushed through its own handle. Recovery tolerates a generation file that
    did not survive by falling back to the previous one, which is still
    present until the new one has been flushed.
- **Handles close before deletion.** Go opens files on Windows without
  `FILE_SHARE_DELETE`, so the recorder closes every handle it holds on an
  output or ticket (reservation, sync helper, generation writer) before that
  file is deleted or replaced.
- **New directories** (outbox, destination, spool) are synced to their parent
  once when they are created, on Linux.

### 4.4 Uploader: one per manager, owns all upload I/O

- **One worker** takes the oldest ticket whose next attempt is due; each
  transfer already sends up to five multipart parts in parallel. Backoff goes
  from 1 minute up to a cap of 1 hour, with no abandonment (R3).
- **Overdue alerting.** A recording becomes overdue two hours after it was
  finalized and became ready (D2). A single "uploads overdue" alert covers the overdue set. It
  clears on delivery or on an explicit discard.
- **Shutdown is owned by the manager.**
  - Uploads run from `Manager.StartUploads` until the process-lifetime
    `Manager.Close`; `Manager.Stop`, which an input change also calls, leaves
    them running. An interrupted upload leaves its ticket in state `ready`.
    This replaces the `uploadStoppedReason` sentinel.
  - `Manager.Close` cancels the uploader and joins the worker and the
    multipart aborts, bounded by `FailTimeout` (5 s) plus a margin. It runs
    before the event log and the alert orchestrator close (`main.go` shutdown
    order, #518).
  - A bucket lifecycle rule for incomplete multipart uploads goes in the
    install docs.
- **Recorder stop and removal do no upload work.** I5 therefore holds by
  construction.
- **S3 clients** are created per upload from the destination's stored
  credentials, which is cheap; S3 retention keeps its recorder client.

### 4.5 Retention

- Local retention skips every file that has a ticket in any state (I9).
- S3 retention stays as it is: owned namespaces, name time and
  `LastModified`.
- Failed and lost tickets wait for the operator. Their disk use shows up in
  the storage alert, and `/ready` stays red while they exist.

### 4.6 Health

- Errors are sticky until the next successful segment or an operator action
  (#514).
- A storage path is ok, low or unavailable (a failed probe); `Low` and `Error`
  already express that, so no new type is needed (#511):
  - a path that was low or unavailable stays low until it has the recovery
    margin free, so a probe error during a low incident cannot fake a
    recovery;
  - a switch between low and unavailable alerts, since freeing space and
    fixing a mount are different jobs;
  - #506's recovery margin is kept.
- The first draft had one `Manager.Health()` snapshot for `/ready`, the
  WebSocket status and alerts. It was dropped (YAGNI): those read recorder
  statuses, the upload backlog and storage separately, each already
  consistent, and no defect came from that.

### 4.7 Encoder coupling (R1)

- The recording manager starts once, when the encoder first starts. It stops
  only on an operator stop or process exit. Input changes and source restarts
  no longer call `recordingManager.Stop()`.
- **Input outages are filled with silence (D1).** A file's playback position
  then equals wall-clock time: minute 37 of the 14:00 file is 14:37.
  - The writer goroutine of a segment tracks the audio it has written against
    the time since the segment started.
  - When no PCM has arrived for longer than a gap threshold (proposed 1 s,
    well above the capture chunk interval), the writer writes silence (zero
    samples in the capture format) in 100 ms blocks until capture resumes.
  - Only outages are filled. The writer does not correct drift between the
    capture clock and the wall clock.
  - A segment holding nothing but filler is discarded like today's empty
    file (#440), so an hour in which the input never came back leaves no
    silent file. The segment therefore counts real and filler bytes
    separately.
  - The start and end of a filled gap are logged as recorder events with
    their duration.

### 4.8 What stays and what goes

Mechanisms that proved themselves and stay:
- the O_EXCL reservation with the SameFile ownership check (#479);
- ID-based file prefixes (#481);
- the persisted upload identity with `If-None-Match: *` and HEAD verification
  (#443);
- the clock-sync gate (#431);
- the bounded capture buffer and writer goroutine (#299, #441);
- make-before-break rotation (#418);
- the storage hysteresis margin (#506).

Mechanisms that go:
- `startGen`, `rotationGeneration`, `durationGeneration` and `restoreGen`;
- `retiring` and `retiringFiles`;
- `uploadQueue`, `retryQueue`, `retryInFlight`, `retryProcessMu`,
  `retryEnqueueMu`, `retryRequested`, `uploadClosed`, `cancelUploads` and
  `uploadStoppedReason`;
- `reconcileMu` and the stop-done channel from #508, which the command loop
  subsumes;
- the `.unfinalized` marker and the chmod-0400 failure flag;
- the abandoned map and `scanAbandonedUploads`;
- the `prepareLocks` map, which is never pruned.

`usedFilenames` stays until a corpus scenario proves O_EXCL plus
If-None-Match cover name reuse after an S3-only spool file is deleted.

## 5. Open issues mapped to the design

| Issue | Resolved by |
|---|---|
| #486 event I/O under lock | 4.1 status snapshot, no I/O in the loop |
| #488 pending is 0 during upload | 4.2 counts from the outbox index |
| #495 spool dirs use the raw ID | 4.2 recorder key from `RecordingFilePrefix()`; the name-collision validation needs its own fix |
| #496 S3-only quarantine invisible | 4.2 failed tickets counted and alerted |
| #502 removed recorder's uploads invisible | 4.2 removed recorders keep delivering and counting |
| #511 storage probe error and hysteresis | 4.6 storage health |
| #512 output settings not applied | 4.1 immediate make-before-break rotation (D3) |
| #514 finalize error cleared on stop | 4.6 sticky errors |
| #515 exported `Start` bypasses #508's guards | obsolete: commands only, same checks in the loop |
| #517 queued uploads lost in local+S3 | 4.2 ticket at reservation |
| #518 retry pass cut off by exit | 4.4 `Manager.Close` joins the uploader |
| #523 interrupted local+S3 file never uploaded | 4.2 recovery of `recording` tickets |
| #490, #513 | out of scope (event log, frontend); fix independently |

## 6. Verification

The result is judged by evidence, not by review alone.

### 6.1 The invariant corpus

The corpus lives in `internal/recording/invariants_test.go`, in this PR.

- **It is black-box.** It uses the `Manager` API, a fake FFmpeg that copies
  PCM verbatim (streaming its output in the crash scenarios), and a fake S3
  store. The store refuses a PUT without
  `If-None-Match: *`, counts committed writes, and can be up, refusing,
  hanging, conflicting or losing a response. It also uses the filesystem and a
  child process that is SIGKILLed to simulate a crash, either alone or with
  its whole process tree. The harness waits until the whole process group is
  gone. Crash scenarios need process groups and skip on Windows.
- **It needs a few seams,** in `export_test.go`: manual timers, waiting until
  the recorders have acted on them or on a configuration change, an
  immediate retry pass, the uploader's clock, and child scenarios. The
  redesign must keep offering them.
- **Known violations were a ratchet.** Until step 3, a scenario known to fail
  on main was skipped only when its violations matched the expected ones,
  and failed once it started to hold. With the last entry (#514) gone, the
  ratchet was removed: every scenario must hold.

Results on main `ccb54e1`:

| Scenario | Result |
|---|---|
| RoundTrip/local, s3, both | holds |
| ExactlyOnce/ForeignObject, LostResponse | holds |
| BoundedStop/StopRecorder, ManagerStop | holds (violated before #507: Stop > 5 s) |
| HonestPending/UploadFailed | holds |
| HonestPending/UploadInFlight | violated (#488): 0 in 50/50 samples while a retry PUT is held |
| HonestPending/RemovedRecorder | violated (#502) |
| ClosedLifecycle/StartAfterStop | holds (violated before #508, #484) |
| ConfigApplies/Disable, Enable | hold (violated before #508, #487) |
| ErrorStaysVisible/FinalizeFailure | violated (#514) |
| Durable/{service,tree}/QueuedFinalized/s3, InterruptedActive/s3 | hold |
| Durable/{service,tree}/QueuedFinalized/both | violated (#517): exactly hours 2–3 never reach S3 |
| Durable/{service,tree}/InterruptedActive/both | violated (#523): hour 4 never reaches S3 |

Mutation checks show that the corpus notices both breakage and fixes:

| Change to production code | Corpus reaction |
|---|---|
| Remove `If-None-Match` | RoundTrip/s3 and both ExactlyOnce scenarios fail |
| Skip the HEAD identity check | ExactlyOnce/LostResponse fails: 2 objects from 2 writes |
| Count `retryQueue + retryInFlight` (#488 fix shape) | UploadInFlight flips to "now holds"; RemovedRecorder stays known |
| Persist upload metadata when queueing (#517 fix shape) | QueuedFinalized/both flips; InterruptedActive/both stays known |
| Disable all local+S3 recovery (also loses hour 1) | QueuedFinalized/both fails instead of skipping |
| Main before #507 / before #508 | BoundedStop violated / ClosedLifecycle and ConfigApplies violated |

Known limits of the corpus:
- Power loss is not modelled. The kernel keeps everything written, so the
  durability watermark is untested until the fault-injection seam exists
  (6.2).
- Files are small, so multipart uploads are not exercised.
- The crash scenarios do not run on Windows.

### 6.2 Next steps

1. **Close the corpus gaps** as the redesign adds seams:
   - an injected clock for I9 and I10;
   - crash checkpoints at every outbox transition and every fsync, using the
     child-process harness;
   - an encoder-level scenario for I11;
   - multipart upload and abort.
2. **Randomized schedules.** A property test generates random sequences of
   start, stop, update, rotate, remove, crash, S3 up and S3 down. It checks
   I2–I8 after every step and logs the seed.
3. **Soak on real hardware,** 72 hours on a Raspberry Pi with an SD card and on
   a Windows PC.
   - Inject faults: scripted S3 outages (firewall rule), `kill -9` every few
     hours, real power cuts on the Pi, and a full disk.
   - Afterwards an audit script compares the expected hourly files with the S3
     objects: present, non-empty, exactly once, and contiguous apart from
     logged outages. After a power cut, the loss must not reach back past
     the durability watermark recorded in the ticket.
4. **Independent review** of this design and of every implementation PR.

## 7. Plan

No migration (R4), so each step replaces code behind the existing `Manager`
API, plus a new `Close`:

1. This PR: the design and the corpus.
2. Outbox, durability protocol and uploader with `Manager.Close`. They replace
   the upload queue, retry, spool recovery and the abandoned scan, and they
   flip #488, #502, #517, #523 and #518.
3. The recorder loop. It replaces the lifecycle guards, flips #514 and keeps
   I7. Delete the white-box tests the corpus covers, but only after the corpus
   covers them.
4. Storage health (#511). The health snapshot was dropped (4.6).
5. Encoder decoupling (R1) and silence fill (D1).
6. Corpus gaps (6.2, item 1), randomized schedules, then the soak.

## 8. Decisions

Taken on 2026-10-22, after the first draft:

- **D1.** Input outages are filled with silence, so a file's playback time
  equals wall-clock time (4.7).
- **D2.** An undelivered recording is overdue, and alerted, two hours after it
  became ready for upload (4.4).
- **D3.** Saving new output settings starts a new segment immediately, with a
  make-before-break rotation (4.1). R1 includes this cut.
- **D4.** While recording, the output is synced every W = 30 seconds (4.3).
  This is a balance between SD-card wear and the audio at risk on power loss;
  the soak on the Pi measures what it costs.
- Removed recorders keep delivering their recordings (R2).
