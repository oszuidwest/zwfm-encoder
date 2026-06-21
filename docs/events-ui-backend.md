# Events UI - Backend Implementation Plan

Handover for implementing backend support for the redesigned Settings > Events
page (the "Issues Inbox / All Events" direction). Read this top to bottom; it
states what the backend does today, what the new UI needs, and exactly what to
change - and, just as important, what NOT to change.

## TL;DR

The goal is a **DRY split where the backend owns the *meaning* of an event and
the frontend stays thin** (presentation + correlation only). See the mental
model in the next section.

- **P1 (required for the DRY goal): the backend classifies every event and the
  API serves it.** `internal/eventlog` gets a canonical `type -> severity /
  category / reason` classifier; `GET /api/events` decorates each event with
  those fields; `web/app.js` then reads them and **drops its own**
  `EVENT_SEVERITY` map and `getEventCategory()` inference. This is the backbone -
  it is what makes the system DRY (one home for "what an event means", next to
  the `type` definition) instead of the meaning living in 2-3 places.
  - Nuance, stated honestly: a barely-working UI needs *almost no* backend change
    (the frontend could keep inferring severity/category as it does today). The
    earlier draft optimised for "minimum to ship". Once the goal is "DRY, thin
    frontend", that frontend inference is exactly the duplication we are removing,
    so P1 becomes required, not optional.
- **P2 (recommended): surface pending S3 uploads per recorder** in the WebSocket
  `status` payload. The aggregate is already in `/ready`; what is missing is the
  per-recorder signal in the status the events page consumes.
- **The hard line that keeps this DRY *without* backend state: no server-side
  incident engine, no `/api/incidents`, no classification stored in the JSONL.**
  The backend owns the stateless *vocabulary* (a pure function of `type`); it does
  NOT own correlation. Pairing a problem with its recovery, folding retries, and
  deciding "still ongoing" stay in the frontend, fed by the API view + the live
  `status`/`levels` state (which already answers "is it broken right now", via
  `/ready` - see section 1.4, not `/health`).

The reference UI lives in `web/prototypes/` (`11-issues-inbox.html`,
`21-all-events.html`, data + helpers in `_data.js`).

---

## 0. Mental model - four layers, each owns exactly one thing

The whole design is "put each fact in exactly one place." Four layers, no
overlap:

| Layer | Owns | Contains | Must NOT contain |
|-------|------|----------|------------------|
| **JSONL log** (`encoder.jsonl`) | raw historical facts | `ts`, `type`, `stream_id`, `msg`, `details` | classification (derivable -> would duplicate + bloat) |
| **`GET /api/events`** | the *semantic view* of those facts | raw fields **plus** `severity`, `category`, `reason`, computed at serve time from `type` | correlation, incidents, live state |
| **WS `status` / `levels`** | the *live truth* - "is it broken right now?" | stream down/retrying, silence/imbalance active, pending uploads | history (it is a snapshot, not a log) |
| **Frontend** (`web/app.js`) | presentation + correlation | grouping by `reason`, folding retries into incidents, pairing problem+recovery, the Needs-attention / Resolved / Activity / Routine sections, labels/icons/text | re-deriving meaning (reads it from the API) or inventing live state (reads it from WS) |

Two single-sources-of-truth fall straight out of this:

- **Meaning** lives once, in the backend, next to the `type` constants, and is
  read by every consumer through the API. (DRY: the web UI, a future CLI, a
  dashboard all get identical severity/category/reason.)
- **Live state** lives once, in the running encoder, and is published through
  `status`/`levels`. The log cannot answer "still happening" (an end event may
  have rotated away); the runtime can.

The frontend is then "dumb on purpose": it never decides what an event *means*
or whether something is *currently* broken - it only arranges what the backend
already told it. That is the DRY win, and it is achieved with **zero** new
backend state: classification is a pure function, correlation stays a view
concern.

Why not push correlation into the backend too? Because correlation is stateful
(it needs a window of events + the live state) and would force an
`/api/incidents` endpoint with its own lifecycle and a second source of truth.
Classification is the opposite - stateless, cheap, pure - so it is the right
thing (and the only thing) to move server-side.

---

## 1. How the backend emits events today

### 1.1 Event model

Source of truth: `internal/eventlog/logger.go`.

- One `Event` per line in a JSON Lines file (`encoder.jsonl`), rotated at 50 MiB
  with one `.1` backup (`logger.go`, `DefaultMaxLogSizeBytes`).
- `Event` shape (`logger.go:70`):

  ```json
  { "ts": "RFC3339", "type": "stream_error", "stream_id": "...",
    "msg": "Stream failed", "details": { ... } }
  ```

- There are exactly **20 event types** in three groups (`logger.go:21-68`):
  - Stream (5): `stream_started`, `stream_stable`, `stream_error`,
    `stream_retry`, `stream_stopped`.
  - Audio (5): `silence_start`, `silence_end`, `audio_dump_ready`,
    `channel_imbalance_start`, `channel_imbalance_end`.
  - Recorder (10): `recorder_started`, `recorder_stopped`, `recorder_error`,
    `recorder_file`, `upload_queued`, `upload_completed`, `upload_failed`,
    `upload_retry`, `upload_abandoned`, `cleanup_completed`.
- Per-group detail structs (`logger.go:79-120`): `StreamDetails`,
  `SilenceDetails`, `ImbalanceDetails`, `RecorderDetails`. Field reference is in
  `docs/events.md`.

### 1.2 Emission sites

Only stream events carry a human `msg`: `LogStream` is the sole caller that sets
`Event.Message` (`internal/eventlog/logger.go:264`). `LogSilenceStart` /
imbalance / dump (`:284`) and `LogRecorder` (`:369`) leave `Message` empty and
populate only `Details`. So in the table below, the last column is the `msg` for
stream rows and a summary of `details` fields for the audio/recorder rows.

| Type | Where emitted | `msg` (stream) / `details` (audio, recorder) |
|------|---------------|----------------------------------------------|
| `stream_started` | `streaming/manager.go:283` (caller), `:369` (listener) | msg `Connecting to <ep>` / `Listening on <ep>` |
| `stream_stable` | `streaming/manager.go:150` | msg `Stream connected and stable` |
| `stream_error` | `streaming/manager.go:919`, `:1148` | msg `Stream failed` / `Listener encoder failed` |
| `stream_retry` | `streaming/manager.go:992` (caller), `:1077` (listener encoder) | msg `Retrying in <d>` (caller) / `Retrying encoder in <d>` (listener); details `retry`/`max_retries` (default 99) |
| `stream_stopped` | `streaming/manager.go:569`, `:901`, `:905` | msg `Stream stopped by user` / `Stream ended normally` |
| silence / imbalance / dump | `notify/notifier.go:277-330` | details only: level/threshold/duration |
| recorder lifecycle | `recording/recorder.go:262,275,347,585` | details only: codec / storage_mode / error |
| uploads + cleanup | `recording/recorder_upload.go:59-411`, `cleanup.go:217` | details only: filename / s3_key / error / retry |

Notes that the UI depends on:
- `upload_abandoned` is terminal: it fires when the retry **age** exceeds 24h
  (`now.Sub(firstAttempt) > MaxUploadRetryAge`, `recorder_upload.go:330`,
  `MaxUploadRetryAge = 24h` `:36`), NOT at a fixed retry count. The `retry` field
  is per-pending-upload and diagnostic: it depends on how often retry processing
  ran, and it is persisted in the spool metadata (`RetryCount` in
  `pendingUploadMetadata`, `internal/recording/spool.go:26`) so it survives
  restarts. Treat it as diagnostic only - do not show it as "24 tries". It also
  raises an **external alert**
  (`notify/notifier.go:207`); together with `silence`, it is the only thing the
  encoder itself escalates to a human - a good signal for what counts as
  "notable".
- `recorder_error` carries a human error (e.g. `local path is not writable`).

### 1.3 How events are served

- Read path: `eventlog.ReadLast(path, n, offset, filter)` (`logger.go:422`) -
  reads both the active and rotated files in reverse, newest-first, applies a
  category filter, returns `([]Event, hasMore, err)`. `n` capped at
  `MaxReadLimit = 500`.
- HTTP: `GET /api/events?limit=&offset=&type=` -> `handleAPIEvents`
  (`api.go:1183`, route `server.go:255`). `type` is one of
  `stream|audio|recorder` (else all). Response: `{ "events": [...],
  "has_more": bool }`. Events are returned **verbatim** - no severity, category,
  or any derived field.
- Category classification already exists in the backend as
  `matchesFilter` + `IsStreamEvent` / `IsSilenceEvent` /
  `IsChannelImbalanceEvent` / `IsRecorderEvent` (`logger.go:548-...`). This is
  the natural home to extend (see P1).

### 1.4 Live runtime state already exposed (this is the important part)

The "Needs attention" section of the new UI is about what is **happening right
now**, which is **not** something you can read reliably from an append-only log
(a `silence_start` with no `silence_end` in the page could be ongoing, rotated
away, or pre-restart). The backend already tracks and publishes the live truth:

- WebSocket `status` every 3s - `WSRuntimeStatus` (`types/types.go:483`,
  built in `server.go:192`):
  - `StreamStatus  map[string]ProcessStatus`
  - `RecorderStatuses map[string]ProcessStatus`
  - `ProcessStatus` (`types/types.go:50`): `State` (`stopped|disabled|starting|
    running|rotating|stopping|error`), `Stable`, `Exhausted`, `RetryCount`,
    `MaxRetries`, `Error`, `Uptime`, `AudioDrops`, ...
- WebSocket `levels` every 100ms - `AudioLevels` (`audio/types.go:16`):
  `SilenceLevel == "active"` while input is silent, `ChannelImbalanceLevel ==
  "active"` while imbalanced, plus durations and L/R dB.
- `/ready` (not `/health`) already derives broadcast-readiness from exactly
  these signals: `handleReady` (`api.go:987`) -> `buildReadyResponse`
  (`api.go:1016`) with components `readyStreams` (`api.go:1056`), `readySilence`
  (`api.go:1088`), `readyChannelImbalance` (`api.go:1100`), `readyRecorders`
  (`api.go:1112`), `readyUploads` (`api.go:1151`).
  - `/health` deliberately does NOT treat these as failures: `buildHealthResponse`
    (`api.go:963`) is healthy when `ffmpegAvailable && encoder State == running`,
    and reports silence / imbalance only as informational booleans
    (`SilenceDetected` / `ChannelImbalanceDetected`). The code comment at
    `api.go:961` spells this out. Anywhere the UI mirrors readiness, mirror
    `/ready`.

So the frontend can render an authoritative "ongoing" section by combining the
WS `status` + `levels` it already receives. No new endpoint needed for that.

---

## 2. What the new UI needs

The UI (`web/prototypes/_data.js`, `21-all-events.html`) groups events as:

1. **Needs attention** - unresolved / ongoing / failed (pinned top).
2. **Resolved** - a problem and its recovery folded into one incident.
3. **Activity** - lifecycle changes (start / stop / connect).
4. **Routine** - the hourly heartbeat, summarised to one line.

To do that it needs, per event type:

- **category**: `stream | audio | recorder` (already in backend).
- **severity**: `error | warning | success | info`.
- **reason**: `problem | recovery | lifecycle | routine`. `routine` is the
  hourly heartbeat (`recorder_file`, `upload_queued`, `upload_completed`,
  `cleanup_completed`) - ~95% of production volume that must collapse by default.

And it needs **live "ongoing" state** (from section 1.4), mapped to incidents.

### Canonical classification table (implement exactly this)

| type | category | severity | reason |
|------|----------|----------|--------|
| stream_started | stream | info | lifecycle |
| stream_stable | stream | success | recovery |
| stream_error | stream | error | problem |
| stream_retry | stream | warning | problem |
| stream_stopped | stream | info | lifecycle |
| silence_start | audio | warning | problem |
| silence_end | audio | success | recovery |
| audio_dump_ready | audio | info | lifecycle |
| channel_imbalance_start | audio | warning | problem |
| channel_imbalance_end | audio | success | recovery |
| recorder_started | recorder | info | lifecycle |
| recorder_stopped | recorder | info | lifecycle |
| recorder_error | recorder | error | problem |
| recorder_file | recorder | info | routine |
| upload_queued | recorder | info | routine |
| upload_completed | recorder | success | routine |
| upload_failed | recorder | error | problem |
| upload_retry | recorder | warning | problem |
| upload_abandoned | recorder | error | problem |
| cleanup_completed | recorder | success | routine |

`subsystem` for the UI maps 1:1 from category: stream -> Streams, audio ->
Audio, recorder -> Recording. `IsRoutine == (reason == routine)`.

Where this lives today (be precise):
- **severity + category** are already inferred in production `web/app.js`:
  severity via the `EVENT_SEVERITY` map (`:86`), category via `getEventCategory()`
  (`:1906`) using the `isAudioEventType` / `isRecorderEventType` helpers (`:333`,
  `:339`). (`EVENT_CATEGORY_BADGE` `:103` is presentation only - it maps an
  already-inferred category to its S/A/R letter, it does not infer.) Severities
  are also documented in `docs/events.md` ("Severity Levels"). So these are
  duplicated FE/docs vs the backend taxonomy.
- **reason + routine** do not exist in the backend or production frontend at
  all; they live only in the prototype `web/prototypes/_data.js` (~`:96`).

P1 therefore (a) canonicalises severity/category in the backend so they stop
being FE-inferred, and (b) adds the new reason/routine taxonomy.

Note: `reason` classifies the event TYPE; it does not by itself decide the UI
section. A `recovery` that does not close an open incident - most often the
normal `stream_stable` right after a fresh `stream_started` - is rendered as
**Activity**, not as a recovery (see the prototype `buildIncidents` /
`activity` filter, `web/prototypes/21-all-events.html:183,337`). Section
assignment = `reason` + incident correlation, done in the frontend.

---

## 3. Backend changes

### P1 (REQUIRED) - Canonical event classification, served on the API

**Goal:** one source of truth for severity/category/reason, next to the
`EventType` constants, surfaced on the API so the frontend stops guessing.
Required for the DRY architecture: without it the taxonomy stays duplicated in
`web/app.js`. The full deliverable is backend classifier **plus** the frontend
switch-over in step 3 - the backend half alone is not "done", it just moves the
duplication.

Reason needs only three fields on the wire: `severity`, `category`, `reason`. Do
NOT add a separate `is_routine` field - it is exactly `reason == "routine"`, and
a second field would re-introduce the duplication P1 removes. The frontend
computes `isRoutine` from `reason`.

**Important:** classification is a pure function of `type`. Do **not** persist it
into the JSONL file (it is derivable and would bloat every line). Compute it at
**serve time** and decorate the API response only.

1. New file `internal/eventlog/classify.go`:

   ```go
   package eventlog

   import "slices" // for AllEventTypes(); Go 1.21+ stdlib

   type Severity string
   const (
       SeverityError   Severity = "error"
       SeverityWarning Severity = "warning"
       SeveritySuccess Severity = "success"
       SeverityInfo    Severity = "info"
       SeverityUnknown Severity = "unknown" // default arm only; never a real event's value
   )

   type Reason string
   const (
       ReasonProblem   Reason = "problem"
       ReasonRecovery  Reason = "recovery"
       ReasonLifecycle Reason = "lifecycle"
       ReasonRoutine   Reason = "routine"
       ReasonUnknown   Reason = "unknown" // default arm only; never a real event's value
   )

   type Category string
   const (
       CategoryStream   Category = "stream"
       CategoryAudio    Category = "audio"
       CategoryRecorder Category = "recorder"
       CategoryUnknown  Category = "unknown" // never silently bucket an unknown type
   )

   // Category deliberately reuses the existing Is*Event helpers - they are
   // already the category source of truth that matchesFilter uses, so this stays
   // DRY rather than duplicating the grouping as a 20-case switch. An
   // unrecognised (future) type returns CategoryUnknown rather than being
   // silently bucketed as recorder. (Because this is a tagless switch on helper
   // results, the exhaustive linter does not apply to it - only to the Severity
   // and Reason switch-on-t below.)
   func (t EventType) Category() Category {
       switch {
       case IsStreamEvent(t):
           return CategoryStream
       case IsSilenceEvent(t), IsChannelImbalanceEvent(t):
           return CategoryAudio
       case IsRecorderEvent(t):
           return CategoryRecorder
       default:
           return CategoryUnknown
       }
   }

   // Likewise, Severity and Reason are explicit switches over all 20 types whose
   // default returns the DISTINCT sentinel SeverityUnknown / ReasonUnknown
   // ("unknown"). "unknown" is both distinct from every real value (so the test
   // catches a missing case) AND a meaningful API value (an unrecognised
   // type still gets a backend-owned classification, never an empty string).
   // Crucial: the default must NOT return a legitimate value (e.g. SeverityInfo /
   // ReasonLifecycle) - those are the real values for stream_started,
   // stream_stopped, audio_dump_ready, recorder_started/stopped, so a missing
   // case for one of them would fall through to the default and silently produce
   // the correct-looking value, defeating any value-based test. A sentinel makes
   // a missing case observable.
   func (t EventType) Severity() Severity { /* explicit switch; default SeverityUnknown */ }
   func (t EventType) Reason() Reason     { /* explicit switch; default ReasonUnknown */ }
   func (t EventType) IsRoutine() bool    { return t.Reason() == ReasonRoutine }
   ```

   Implement `Severity` and `Reason` as explicit `switch t { case ... }`
   statements over all 20 types; `Category` reuses the `Is*Event` helpers as
   shown above. All three default to the sentinel
   (`SeverityUnknown`/`ReasonUnknown`/`CategoryUnknown`, all `"unknown"`). This is
   deliberately a real, non-empty API value: the `/api/events` contract promises
   backend-owned meaning, so an unknown `type` must still classify to
   `"unknown"` (not `""`). The frontend renders `"unknown"` as a neutral/info row.
   The sentinel is also what makes the test in section 5 able to catch a missing
   case (see there).

   Also add a canonical enumerator next to the constants and use it everywhere
   you need to iterate types (the test, any docs generation):

   ```go
   // Backing array is an unexported [N]EventType so its length is fixed and it
   // cannot be reassigned. Keep in sync with the const blocks above.
   var allEventTypes = [...]EventType{
       StreamStarted, StreamStable, StreamError, StreamRetry, StreamStopped,
       SilenceStart, SilenceEnd, AudioDumpReady, ChannelImbalanceStart, ChannelImbalanceEnd,
       RecorderStarted, RecorderStopped, RecorderError, RecorderFile,
       UploadQueued, UploadCompleted, UploadFailed, UploadRetry, UploadAbandoned, CleanupCompleted,
   }

   // AllEventTypes returns a fresh copy so callers (tests, docs gen) cannot
   // mutate the canonical list.
   func AllEventTypes() []EventType { return slices.Clone(allEventTypes[:]) }
   ```

   Honest about enforcement, two independent gaps:
   - **Wrong/missing classification of a known type** is caught by the
     section-5 expected-value table *because* the default is a sentinel: a
     forgotten `case` yields `"unknown"`, which no row in the table expects.
   - **A 21st constant added but left out of `allEventTypes`** is NOT caught by
     any value test (Go const blocks are not auto-enumerable; the test only sees
     what is in the array). The only structural guarantee is the `exhaustive`
     linter - not enabled in this repo today (`.golangci.yml`). If you adopt it,
     keep `default-signifies-exhaustive: false` (its default) so the runtime
     `default` arm does NOT count as handling the enum; otherwise the linter
     ignores missing cases and you lose the guarantee. `EventType` is a named
     string-enum, which `exhaustive` supports. This covers the `Severity` and
     `Reason` switch-on-`t`; `Category` goes through the `Is*Event` helpers, so
     exhaustive does not apply to it (and the existing partial helpers would
     need excluding if you enable the linter repo-wide).

2. Decorate the response in `handleAPIEvents` (`api.go:1183`). Add a view type
   rather than changing `eventlog.Event`:

   ```go
   type eventView struct {
       eventlog.Event
       Severity eventlog.Severity `json:"severity"`
       Category eventlog.Category `json:"category"`
       Reason   eventlog.Reason   `json:"reason"`
   }

   views := make([]eventView, len(eventList))
   for i, e := range eventList {
       views[i] = eventView{Event: e, Severity: e.Type.Severity(),
           Category: e.Type.Category(), Reason: e.Type.Reason()}
   }
   s.writeJSON(w, http.StatusOK, map[string]any{"events": views, "has_more": hasMore})
   ```

   The three fields are purely additive; existing clients ignore unknown keys,
   so this is backwards-compatible. (JSON object key order is not part of the
   contract, so do not rely on it either way.)

3. Frontend switch-over (REQUIRED part of P1, not a "someday" cleanup): once the
   API ships the fields, `web/app.js` reads `event.severity` / `event.category`
   / `event.reason` and **removes** its own `EVENT_SEVERITY` map (`app.js:86`)
   and `getEventCategory()` inference (`app.js:1906`, with the
   `isAudioEventType` / `isRecorderEventType` helpers). Server and embedded web
   ship from the same build, so there is no version skew - the frontend can
   depend on the fields directly. Treat the `"unknown"` sentinel as neutral in
   the FE (e.g. `severityClass(event.severity)` falls back to the info styling
   for `"unknown"` or any unexpected value). Do NOT keep the full local
   `EVENT_SEVERITY` / category maps, or you have re-created the duplication. This
   can be the same PR or an immediate follow-up, but it is part of "P1 done", not
   optional.

### P2 (recommended) - Surface pending S3 uploads per recorder in the WS status

Correction to the original draft: pending uploads are **not** invisible today.
`/ready` already exposes them as an **aggregate** component: `handleReady`
passes `s.encoder.PendingUploadCount()` (`api.go:999`) to `readyUploads`
(`api.go:1151`), which fails the readiness check when `pending >
readinessMaxPendingUploads`, and that constant is **0** (`api.go:932`) - so any
pending upload already makes `/ready` `not_ready`. The recorder accessor exists:
`GenericRecorder.PendingUploadCount()` (`internal/recording/recorder.go:492`)
returns `len(r.retryQueue)`.

What is genuinely missing for the new UI: (a) **per-recorder** visibility (the
aggregate cannot say which recorder is stuck), and (b) publication in the
**WebSocket `status` payload**, which is what the events page consumes -
`RecorderStatuses` carry `ProcessStatus`, and `ProcessStatus` has no pending
field.

1. Add to `ProcessStatus` (`types/types.go:50`):

   ```go
   PendingUploads int `json:"pending_uploads,omitempty"`
   ```

2. Set it inside `GenericRecorder.Status()` (`internal/recording/recorder.go:480`),
   which already holds `r.mu.RLock()`. Read `len(r.retryQueue)` **directly** -
   do NOT call `PendingUploadCount()` from inside `Status()`, because that
   re-acquires `r.mu.RLock()`, and Go's `sync.RWMutex` documents that a nested
   read lock can deadlock if a writer is waiting in between. `Status()` flows up
   through `(*Encoder).RecorderStatuses()` (`internal/encoder/encoder.go:269`)
   into the `status` WS payload, so no extra wiring is needed.

3. Frontend then treats `recorder_statuses[id].pending_uploads > 0` (the
   snake_case browser key) as an ongoing recorder incident, resolved when it
   drops to 0 or escalated when an `upload_abandoned` event arrives.

This is additive and behind `omitempty`, so it is safe for existing clients.

### What NOT to do

- **No `/api/incidents` and no server-side correlation engine.** Pairing a
  problem with its recovery, folding `stream_retry` bursts, and computing
  durations is view logic over data the client already has (the event page +
  the live status). The prototype does it in ~60 lines
  (`web/prototypes/21-all-events.html`, `buildIncidents`). Putting it server-side
  adds stateful complexity and a second source of truth for no benefit unless
  multiple independent clients must share identical incident IDs (not a current
  requirement).
- **Do not derive "ongoing" from the event log.** Always use the live
  `status` / `levels` state (section 1.4). The log cannot distinguish "still
  happening" from "the end event rotated away".
- **Do not write severity/category/reason into the JSONL file.** Decorate on
  read only.

---

## 4. Frontend contract (so backend and FE agree)

The frontend will assemble the four UI sections like this:

- **Needs attention (live):** built from WS `status` + `levels`, not the log.
  All field names below are the **JSON keys the browser receives** (snake_case),
  NOT the Go struct fields - a FE implementer can use them verbatim. The `status`
  message carries `stream_status` and `recorder_statuses` (maps of id ->
  `ProcessStatus`); the `levels` message carries `levels` (`AudioLevels`).
  - stream incident for **enabled streams only** (skip disabled). The condition
    differs by mode, because `stable` is `true` only for caller streams
    (`Stable: isRunning && !isListener && ...`, `internal/streaming/manager.go:762`):
    - **caller** (`mode != "listener"`): a problem when NOT (`state == "running"
      && stable && !exhausted`), mirroring `readyStreams` (`api.go:1056`).
    - **listener**: do NOT use `stable` (always `false`) and do NOT blanket-skip
      it the way `readyStreams` does - a continuously-restarting listener encoder
      is a real problem the events view should surface. Treat it as a problem
      when `exhausted`, or `state == "error"`, or (running but
      `encoder_running == false`). Use `state` / `exhausted` / `encoder_running`
      / `error`, not `stable`.
    The status payload has no enabled/mode flag, so the FE cross-references the
    stream's config from `/api/config` (which has `enabled` + `mode`) to pick the
    branch. Enrich title/detail with the latest `stream_error`/`stream_retry`
    from the log for that `stream_id`.
  - silence incident if `levels.silence_level == "active"`.
  - imbalance incident if `levels.channel_imbalance_level == "active"`.
  - recorder incident if `recorder_statuses[id].state == "error"` (message in
    `.error`) or (P2) `recorder_statuses[id].pending_uploads > 0`.
- **Resolved / Activity / Routine (history):** from `GET /api/events`, grouped
  by the new `reason` field (plus correlation - see the note in section 2 about
  unconsumed `stream_stable` becoming Activity); routine collapses to a summary
  line; resolved incidents are correlated client-side (problem + matching
  recovery).

Correlation subtleties the FE handles and the backend does not need to:
- An `upload_failed` is resolved by the later `upload_completed` for the same
  recording (because `upload_completed` is classified `routine`, not
  `recovery`). **Key it on `recorder_name` + `filename` (or `s3_key`), not
  filename alone**, and treat it as best-effort: recorder events carry **no
  recorder id** (`RecorderDetails` has `RecorderName`/`Filename`/`S3Key` only,
  `internal/eventlog/logger.go:109`), and only recorder **IDs** are uniqueness-
  validated, not names (`validateRecorders`, `internal/config/config.go:544`),
  so two recorders sharing a name are ambiguous. `s3_key` embeds the recorder
  name in its path, so it keys on name too. Acceptable for a UI hint; do not
  treat the pairing as authoritative.
- Concurrent `silence` and `channel_imbalance` are independent detectors and
  must not be merged into one incident.
- **Listener streams never emit `stream_stable`.** Their encoder failures emit
  `stream_error` ("Listener encoder failed", `manager.go:1148`) and `stream_retry`
  (`manager.go:1077`), but a successful encoder restart just resumes with no
  recovery event (`manager.go:1091-1097`) - by design, a listener confirms no
  remote output. So the "stream incident closes on `stream_stable`" rule is
  **caller-only**. For listener streams the log has no resolution event, so the
  FE must NOT open a historical incident that can only close on `stream_stable`
  (it would hang as eternally "ongoing" - the reference prototype's `CLOSES` map
  at `web/prototypes/21-all-events.html:155` has exactly this gap). Instead:
  drive listener health from the live status (`encoder_running`/`state`/
  `exhausted`, as in section 4) and render past listener `stream_error`/`retry`
  as self-contained activity, never an unresolved incident.
- **Telling caller from listener in history.** Stream events carry
  `details.mode` (`caller` or `listener`) in `StreamDetails`. The frontend uses
  that field directly. There is no message-pattern or current-config fallback;
  logs without `details.mode` should be cleared before running this version.

---

## 5. Testing

- `internal/eventlog/classify_test.go`: an **explicit expected-value table for
  all 20 types** - a `map[EventType]struct{cat Category; sev Severity; rsn
  Reason}` with one row per type - asserting exact equality, NOT a "non-default"
  check. A "non-default" assertion is unsound here: `info`/`lifecycle` are both
  the legitimate values for several types (`stream_started`, `stream_stopped`,
  `audio_dump_ready`, `recorder_started`, `recorder_stopped`) AND would be the
  same values an incorrect default could return - so a missing case could pass.
  With the sentinel default (`"unknown"`) plus exact-match rows, a forgotten case yields
  `"unknown"`, which no row expects, so it fails.
- Assert **set equality** between `AllEventTypes()` and the table keys, not just
  equal lengths: build a `map[EventType]bool` from `AllEventTypes()`, assert its
  length equals `len(AllEventTypes())` (catches a **duplicate** entry) and that
  it equals the table's key set (catches a missing/extra entry). A bare
  `len(table) == len(AllEventTypes())` is fooled by one duplicate plus one
  missing entry, which keep the count the same.
- A separate **unknown-type test**: `EventType("does_not_exist")` returns
  `SeverityUnknown`/`CategoryUnknown`/`ReasonUnknown` (all `"unknown"`).
- Caveat (from P1): none of this auto-detects a 21st constant added without
  touching `allEventTypes`; only the `exhaustive` linter would. Optional.
- `handleAPIEvents`: **add** a handler test (there is no `/api/events` test in
  `api_test.go` today) asserting the three new fields appear and match the type.
  Confirm `has_more` and pagination are
  unchanged.
- P2: a recorder-status test asserting `PendingUploads` reflects queue depth.
- **Prove classification is not persisted.** There is no golden-line fixture
  today - `internal/eventlog/logger_test.go` writes events dynamically and reads
  them back (`writeEvents` / `mustMarshal`, `logger_test.go:171,188`). Two
  things: (a) the existing logger tests must still pass unchanged (the on-disk
  format does not change), and (b) add a small test using the existing
  `mustMarshal` helper asserting the marshalled JSONL line contains `"type"` but
  NOT `"severity"`/`"category"`/`"reason"` - that is the concrete guarantee that
  the fields are serve-time-only.
- **Frontend (P1 touches `web/app.js`):** run `bun run lint` (HTMLHint + Biome)
  and do a quick manual events-view check - load Settings > Events, confirm rows
  still get the right severity colour/category from the server fields and that an
  `"unknown"` value renders neutrally.

## 6. Acceptance checklist

- [ ] `EventType.Severity/Category/Reason/IsRoutine` + `AllEventTypes()`
      (cloned accessor over an unexported array) implemented; all three
      classifiers default to the sentinel `"unknown"`
      (`SeverityUnknown`/`CategoryUnknown`/`ReasonUnknown`) - a real, non-empty
      API value, never a legitimate one; explicit 20-row expected-value table
      test + unknown-type test + set-equality (no duplicates) between
      `AllEventTypes()` and the table keys.
- [ ] `GET /api/events` returns `severity`, `category`, `reason` per event (no
      separate `is_routine`); classification is not persisted in JSONL;
      `has_more`/pagination unchanged.
- [ ] **(P1, required) Frontend switched over:** `web/app.js` reads
      `event.severity`/`category`/`reason` and the local `EVENT_SEVERITY` map +
  `getEventCategory()` inference are removed.
      Without this the taxonomy is still duplicated and P1's DRY goal is unmet.
- [ ] (P2) `ProcessStatus.PendingUploads` populated for recorders and present in
      the `status` WebSocket payload.
- [ ] No new endpoint; no server-side incident state.
- [ ] `bun run lint` passes and a manual Settings > Events check confirms rows
      render correctly from the server fields (and `"unknown"` renders neutrally).
- [ ] A test proves the JSONL line has no `severity`/`category`/`reason` keys.
- [ ] `docs/events.md` updated: document `severity`/`category`/`reason` as
      **`/api/events` response decoration** in the "API Access" section, kept
      explicitly separate from the on-disk JSONL schema ("Common Event
      Structure", `docs/events.md:16`), which does **not** change.
- [ ] Frontend can render "Needs attention" from `status` + `levels`
      (+ `pending_uploads`), cross-referencing `/api/config` for enabled + mode
      to branch caller (uses `stable`) vs listener (uses
      `encoder_running`/`state`/`exhausted`, never `stable`), and the four
      sections from `/api/events`. Listener stream incidents must not hang as
      "ongoing" (no `stream_stable` is ever logged for them).
- [ ] New stream JSONL events include raw `details.mode` (`caller` or
      `listener`) so historical stream rows use the event's mode directly, with
      no message-pattern or config fallback.

## 7. Rough effort

- P1 (required): ~1 day. Backend ~half a day (classify.go + table + handler
  decorate + tests + docs); frontend switch-over ~half a day (read the API
  fields, delete `EVENT_SEVERITY` + `getEventCategory()`, retest the events
  view).
- P2 (recommended): ~half a day (one `ProcessStatus` field + set it in
  `GenericRecorder.Status()` from `len(r.retryQueue)` under the existing RLock +
  test).
- No migration, no config change, no breaking change (additive API fields; FE
  and server ship together).
