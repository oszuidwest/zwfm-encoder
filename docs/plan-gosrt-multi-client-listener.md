# Plan: Multi-Client SRT Listener With GoSRT And Pipe Fan-Out

## Summary

The current local pull listener is a single-client FFmpeg SRT listener. That is
acceptable for a direct service endpoint, but it is not a distribution endpoint:
only one client can be connected to a listener stream at a time.

Use `github.com/datarhei/gosrt` for the SRT listener and SRT subscriber
connections, but do not use GoSRT `PubSub` as the primary fan-out mechanism.
The primary design is:

```text
PCM distributor
    -> one FFmpeg encoder per listener stream
    -> FFmpeg encoded stdout pipe
    -> Go bounded fan-out
    -> GoSRT subscriber connections
    -> many SRT clients
```

This avoids the loopback SRT publisher leg entirely. That removes the extra
internal SRT latency buffer, removes the external publisher role, and removes
the need for a second listener socket.

Caller streams remain unchanged. This plan only changes `mode: "listener"`
streams.

## Current Behavior

Relevant current files:

- `internal/streaming/ffmpeg.go`: builds one FFmpeg output URL per stream.
- `internal/streaming/manager.go`: starts one FFmpeg process per stream, writes
  PCM to its stdin, and monitors the process.
- `internal/types/types.go`: defines stream modes, retry delays, listener
  relisten constants, codecs, and stream validation.
- `internal/config/config.go`: prevents duplicate enabled listener bind
  addresses.
- `internal/encoder/encoder.go`: starts streams and fans PCM chunks to
  `streamManager.WriteAudio`.

Current listener flow:

```text
PCM distributor -> FFmpeg process -> srt://0.0.0.0:port?mode=listener
```

In this model the SRT listener socket belongs to FFmpeg. When a client
disconnects, FFmpeg exits; the manager treats listener exits after the
`ListenerStartFailureWindow` as normal sessions and relistens after
`ListenerRelistenDelay`.

That relisten behavior must not be reused after adding a Go relay. In the new
model, client disconnects no longer stop FFmpeg. A FFmpeg encoder exit is a real
failure and must use the error/retry path.

## Decision

Use pipe fan-out as the primary design.

The earlier GoSRT `PubSub` design would have used:

```text
FFmpeg -> loopback SRT publisher -> GoSRT PubSub -> SRT subscribers
```

That is elegant because it reuses GoSRT's one-publisher/many-subscribers helper,
but it adds a second SRT hop. For this appliance the expected subscriber count is
small and low latency matters, so the internal SRT publisher leg is unnecessary
surface area. The minimum that works is to let FFmpeg encode to stdout and fan
out those encoded bytes in Go.

GoSRT remains useful and required in this design:

- it owns SRT handshakes;
- it handles SRT subscriber connections;
- it provides encryption and live transport;
- it provides `Conn.Write`, which is enough for byte fan-out.

## GoSRT Facts Used By This Plan

Verified upstream behavior:

- `srt.Server` has `Addr`, `Config`, `HandleConnect`, `HandleSubscribe`,
  `Listen`, `Serve`, `ListenAndServe`, and `Shutdown`.
- `ConnRequest` exposes `RemoteAddr()`, `StreamId()`, `IsEncrypted()`,
  `SetPassphrase(...)`, `Accept()`, and `Reject(...)`.
- `Conn` implements network-style I/O, including `Read`, `Write`, `Close`,
  address accessors, deadlines, socket IDs, stream ID, and stats.
- `Conn.Write` accepts arbitrary byte slices and segments them internally to the
  configured payload size. Fan-out can write stdout chunks directly; it does not
  need to split chunks into 1316-byte packets itself.
- `Config.Latency` is a `time.Duration`; URL parsing interprets `latency` as
  milliseconds.
- `Config.Passphrase` accepts 10-80 bytes. This project currently accepts SRT
  passwords of 10-64 characters, so the existing validation fits inside GoSRT's
  range.
- GoSRT is pure Go. It does not add a cgo/libsrt dependency.
- The upstream repository currently shows an MIT license. Confirm this again
  during dependency review before merge.

References:

- <https://github.com/datarhei/gosrt>
- <https://pkg.go.dev/github.com/datarhei/gosrt>
- <https://raw.githubusercontent.com/datarhei/gosrt/main/connection.go>
- <https://raw.githubusercontent.com/datarhei/gosrt/main/server.go>
- <https://raw.githubusercontent.com/datarhei/gosrt/main/config.go>
- <https://ffmpeg.org/ffmpeg-formats.html#mpegts>

## Goals

- Allow multiple clients to read the same local listener stream concurrently.
- Keep one FFmpeg encoder process per listener stream.
- Keep caller-mode stream behavior unchanged.
- Preserve current codec choices and bitrate behavior.
- Preserve current listener bind validation, including specific bind addresses.
- Keep listener streams excluded from `/ready` unless product policy changes.
- Surface listener health as encoder process health plus subscriber count.
- Use bounded queues only; no unbounded memory growth on slow clients.
- Keep shutdown deterministic: every relay, FFmpeg process, writer goroutine,
  reader goroutine, and subscriber goroutine must have a clear owner and exit
  path.

## Non-Goals

- Browser playback.
- HLS, WebRTC, RTSP, or RTMP output.
- Multi-publisher streams.
- Perfect client continuity across FFmpeg encoder restarts.
- Replacing FFmpeg's encoder role.
- Adding new listener bind-host configuration.
- Adding detailed slow-subscriber drop metrics in v1.

Listener streams no longer require FFmpeg SRT support in this design, because
FFmpeg writes encoded bytes to stdout. Caller streams still require FFmpeg SRT
support, so `srtAvailable` and `srtSentinel()` gating must remain for caller
streams.

## Target Architecture

### Connection Model

For a configured listener stream on `0.0.0.0:9000`:

```text
External subscribers:
    srt://encoder-ip:9000?mode=caller
    srt://encoder-ip:9000?mode=caller&streamid=read:<stream-id>
    srt://encoder-ip:9000?mode=caller&streamid=<stream-id>

Internal encoder:
    ffmpeg ... -f <format> pipe:1
```

Go owns the SRT listener socket. FFmpeg never listens for SRT clients and never
dials a local SRT publisher URL.

### Stream ID Handling

There is no publisher role in v1. Every accepted SRT connection is a subscriber.
The Stream ID is advisory only; the relay serves one stream on one port and does
not need stream ID routing.

### Encryption

External subscriber encryption should preserve existing behavior:

- If `stream.Password` is empty, allow unencrypted subscribers.
- If `stream.Password` is set, require encrypted subscriber connections and
  call `req.SetPassphrase(stream.Password)`.
- In `HandleConnect`, reject subscribers when `stream.Password != ""` and
  `!req.IsEncrypted()`. Calling `SetPassphrase` only when `IsEncrypted()` is
  true is not enough; otherwise an unencrypted subscriber can bypass the
  password.
- Reject the connection if `req.SetPassphrase(stream.Password)` returns an
  error.
- Keep `PBKeylen=16` to match current behavior.

With the chosen `srt.Server` API, rejected handshakes are reported to clients as
`REJ_PEER`. GoSRT defines extended reasons such as `REJX_FORBIDDEN`, but
`srt.Server.Serve()` ignores per-request rejection reasons and calls
`req.Reject(REJ_PEER)` for `REJECT`. Use the low-level `Accept2` loop only if
specific rejection codes become a requirement.

### Latency

Current FFmpeg listener URL:

```text
latency=300000
```

FFmpeg interprets SRT latency in microseconds, so this is 300 ms.

In the pipe design, FFmpeg no longer has an SRT output URL for listener streams.
Only the GoSRT subscriber side needs SRT latency configuration:

```go
Latency: 300 * time.Millisecond
```

This removes the internal SRT receiver buffer that the loopback-publisher design
would add.

## Fan-Out Design

Create a package such as `internal/srtfanout` that owns GoSRT details and
subscriber fan-out.

Suggested public API:

```go
package srtfanout

type Config struct {
    StreamID       string
    BindHost       string
    Port           int
    Password       string
    Latency        time.Duration
    MaxClients     int
    Logger         *slog.Logger
}

type Server struct {
    // unexported fields
}

func NewServer(cfg Config) (*Server, error)
func (s *Server) Start() error
func (s *Server) Shutdown()
func (s *Server) Wait() error
func (s *Server) Write(chunk []byte)
func (s *Server) ClientCount() int64
```

`Start` should call `srt.Server.Listen()` first so bind errors are synchronous,
then run `Serve()` in a goroutine. This gives `streaming.Manager.Start` a clear
startup result before it starts FFmpeg.

`Shutdown` should stop `Serve`, close all subscriber connections, and wait for
all subscriber writer goroutines before `Wait` returns. Connected clients should
therefore be released deterministically on stream stop, restart, or config
reload.

### Subscriber State

Each subscriber gets:

- the GoSRT `Conn`;
- one bounded `chan []byte`;
- one writer goroutine that reads from the channel and calls `Conn.Write`.

Use a small bounded queue. Because stdout chunks are read-size dependent, bound
chunk size through the stdout read buffer and queue length:

- stdout read buffer: 4 KiB;
- subscriber queue: 2 chunks.

That caps each subscriber at two pending stdout chunks while still absorbing
normal scheduler jitter. Adjust only after measuring on the Raspberry Pi.

`Server.Write` must never block on a slow subscriber:

1. Copy the FFmpeg stdout bytes into a new immutable chunk.
2. Snapshot the subscriber list under a short lock.
3. For each subscriber, try a non-blocking enqueue.
4. If that subscriber queue is full, drop the oldest queued chunk for that
   subscriber and enqueue the newest chunk.

This keeps every subscriber close to the live edge and prevents one slow client
from blocking FFmpeg stdout or other subscribers. Do not add `client_drops` as a
status field in v1; logging sampled slow-subscriber drops is enough until there
is a real debugging need.

If a subscriber writer gets a `Conn.Write` error, close that subscriber and
remove it from the fan-out set.

When there are no subscribers, `Server.Write` should discard chunks. It must
still continue draining FFmpeg stdout so the FFmpeg process cannot block on a
full pipe.

### Max Clients

Use the minimum tracking that works:

- keep an atomic active client count;
- in `HandleConnect`, reject new subscribers when count is at or above
  `MaxClients`;
- increment around `HandleSubscribe`;
- decrement when `HandleSubscribe` returns.

A small concurrent over-admission window is acceptable for v1.

## FFmpeg Args

Keep caller args unchanged.

Add a listener-pipe args builder, for example:

```go
func BuildListenerPipeArgs(stream *types.Stream) []string
```

The listener-pipe output should use the same codec args and output format as
current listener mode, but write to stdout:

- MP3: `-codec:a libmp3lame -b:a <bitrate> -f mp3 pipe:1`
- Opus: `-codec:a libopus ... -f mpegts -pat_period 0.1 pipe:1`
- PCM: `-codec:a s302m -strict -2 -f mpegts -pat_period 0.1 pipe:1`

MPEG-TS late-join behavior matters in the pipe model. MP3 frames are
self-synchronizing, but Opus and PCM are wrapped in MPEG-TS. A new subscriber can
join in the middle of the byte stream and must see PAT/PMT tables before it can
decode. Set `-pat_period 0.1` explicitly for MPEG-TS listener outputs so clients
do not rely on FFmpeg defaults for program table cadence. Do not add
`-mpegts_flags +pat_pmt_at_frames` unless it is tested with audio-only outputs;
FFmpeg documents that flag in terms of video frames.

Update the FFmpeg process wrapper so listener streams can read encoded stdout.
`StartResult` should expose a stdout reader or a callback path owned by the
streaming manager. Keep stdin ownership unchanged: the stream writer goroutine
continues to feed PCM to FFmpeg stdin.

The stdout reader is latency-sensitive:

- read with a modest buffer, for example 4-16 KiB;
- use 4 KiB as the v1 default;
- forward each successful read to fan-out immediately;
- do not use `io.ReadFull` or wait for large blocks to fill.

If the implementation uses `cmd.StdoutPipe`, the stdout reader must drain until
EOF and coordinate with process `Wait`. Do not call `Wait` in a way that closes
the pipe while the reader still expects data; on stop, terminate FFmpeg, let the
reader observe EOF/error, then wait for the encoder-run goroutines.

## Stream State Model

The existing `Stream.state` currently represents one FFmpeg process. With a
fan-out server, listener streams need to distinguish relay state from encoder
process state.

Recommended model:

```text
Stream.state
    caller: current process state, unchanged
    listener: SRT fan-out server state

Stream.encoderRunning
    listener only: FFmpeg encoder process state
```

Suggested fields on `streaming.Stream`:

```go
fanout         *srtfanout.Server
encoderMu      sync.RWMutex
encoder        *encoderRun
encoderRunning atomic.Bool
encoderLastErr string
```

Each FFmpeg encoder process is an encoder run:

```go
type encoderRun struct {
    result    *ffmpeg.StartResult
    audioCh   chan []byte
    wg        sync.WaitGroup
    startedAt time.Time
}
```

Use the existing `sync.WaitGroup` style rather than a separate `writerDone`
channel. The run owns both goroutines:

- stdin writer: drains `audioCh` and writes PCM to FFmpeg stdin;
- stdout reader: reads encoded bytes from FFmpeg stdout and calls
  `fanout.Write`.

`Stream.result` should remain caller-only. Listener encoder process state lives
in `encoderRun`.

Never close an `audioCh` while `WriteAudio` can send to it without holding the
same lock. `WriteAudio` should snapshot the current encoder run under
`encoderMu.RLock` and use a non-blocking send.

`ProcessStatus` should gain listener-specific optional fields:

```go
EncoderRunning bool  `json:"encoder_running,omitempty"`
ClientCount    int64 `json:"client_count,omitempty"`
```

Keep `Stable=false` for listener streams unless product policy changes.

## Manager Changes

### Split Start Paths

Replace the single `Manager.Start` flow with mode-specific internals:

```go
func (m *Manager) Start(stream *types.Stream) (bool, error) {
    if stream.ModeOrDefault() == types.StreamModeListener {
        return m.startListenerFanout(stream)
    }
    return m.startCaller(stream)
}
```

Keep the existing placeholder pattern:

- Claim stream ID under `m.mu` with `ProcessStarting`.
- Preserve retry state.
- Release lock before stopping old resources or starting external processes.
- Re-check placeholder identity before publishing the new stream instance.

Make the FFmpeg SRT capability gate mode-aware. The current encoder startup path
checks `srtAvailable` before starting every stream. After this change:

- caller streams still require FFmpeg SRT support;
- listener streams do not require FFmpeg SRT support, because FFmpeg writes to
  `pipe:1` and GoSRT handles SRT subscribers.

This should be visible in startup behavior: on a system where FFmpeg lacks the
`srt` protocol, listener streams can still start while caller streams fail with
the existing SRT sentinel.

### Lock Ordering

The manager-wide lock and listener encoder lock must have a fixed order:

1. `m.mu`
2. `stream.encoderMu`

Preferred pattern: use `m.mu` only to find or swap the `*Stream`, release it,
then take `encoderMu` for encoder-run work. If nested locking is unavoidable,
take `m.mu` before `encoderMu`. Never take `m.mu` while holding `encoderMu`.

Do not hold `m.mu` across:

- FFmpeg start/stop/wait;
- reads from FFmpeg stdout;
- writes to FFmpeg stdin;
- fan-out `Shutdown` or `Wait`;
- SRT `Conn.Write`;
- sleeps/backoff timers.

### Listener Startup Order

For listener streams:

1. Validate stream.
2. Claim placeholder.
3. Stop old listener stream outside lock:
   - detach and close the active encoder run;
   - wait for its goroutines;
   - cancel/close its FFmpeg process;
   - shut down fan-out and wait for the server goroutine to exit.
4. Start GoSRT fan-out server.
5. Create the real `Stream` instance with fan-out, retry state, no active
   encoder run yet, and metrics.
6. Swap placeholder with real stream under lock.
7. Start the first encoder run:
   - create a new `audioCh`;
   - start FFmpeg with listener-pipe args;
   - start the stdin writer goroutine;
   - start the stdout reader goroutine;
   - publish the run under `encoderMu`.
8. Start listener encoder monitor goroutine.
9. Emit `stream_started` with "Listening on host:port".

If fan-out starts but the first encoder run fails to start, stop fan-out before
returning an error or publish a `ProcessError` stream that owns and shuts down
fan-out. Do not leave an untracked SRT listener bound to the UDP port.

### Monitor Semantics

This is a must-fix item.

The current `classifyStreamExit` treats listener exits after
`ListenerStartFailureWindow` as `streamExitListenerRelisten`. That is correct
only while FFmpeg owns the listener socket and exits normally after a client
session.

In the new fan-out model, FFmpeg encoder exits are real failures:

- source failure;
- FFmpeg crash;
- pipe read/write failure;
- encoder/config mismatch.

Therefore:

- Remove `streamExitListenerRelisten` for fan-out-backed listeners.
- Do not use `ListenerRelistenDelay` for encoder exits.
- Do not reset retry count merely because a listener-mode encoder ran for more
  than one second.
- Use the same backoff and max-retry behavior as caller streams.
- Keep intentional stops classified as intentional stops.

`ListenerStartFailureWindow`, `ListenerRelistenDelay`,
`ListenerRelistenWarningWindow`, `ListenerRelistenWarningThreshold`, and
`listenerRelistenWarningState` become dead code unless another FFmpeg-owned
listener path remains. Remove them when the old listener implementation is
deleted.

### Retry Flow For Listener Encoders

The listener encoder monitor should:

1. Wait for the active FFmpeg encoder process.
2. Mark `encoderRunning=false`.
3. Under `encoderMu`, detach the finished run so `WriteAudio` stops writing to
   it.
4. Close that run's `audioCh` and wait for its goroutines.
5. Record the error and emit `stream_error` if the exit was not intentional.
6. Apply retry/backoff/max-retry rules.
7. Start a new encoder run after backoff while keeping the SRT listener bound.
8. Restore `encoderRunning=true` only after the run is published under
   `encoderMu`.

The listener monitor must not call `m.Start(stream)` for encoder retries.
`Manager.Start` owns SRT listener binding and would rebind the UDP port. Add
encoder-only entrypoints such as:

```go
func (m *Manager) startListenerEncoderRun(s *Stream) (*encoderRun, error)
func (m *Manager) restartListenerEncoder(s *Stream, reason error)
```

If max retries are exceeded:

- set stream state to `ProcessError`;
- detach and close the active encoder run if needed;
- shut down fan-out;
- leave status with final error and exhausted retry state.

### Stop And Remove

Stop order for listener streams:

1. Mark stopping under lock.
2. Under `encoderMu`, detach the active encoder run.
3. Close the detached run's `audioCh`.
4. Cancel/close the detached FFmpeg process.
5. Wait for FFmpeg process and run goroutines.
6. Shutdown GoSRT fan-out.
7. Wait for fan-out goroutine to return.
8. Remove stream from map.

`StopAll` must wait for fan-out shutdowns. A closed SRT listener must release
the UDP port before a later `Start` can bind it again.

## API And UI Changes

### API Status

Extend stream status responses with listener-specific state:

- `encoder_running`
- `client_count`

For listener streams, read `client_count` directly from
`stream.fanout.ClientCount()` in `Statuses()`. Do not mirror this value into a
second atomic on `streaming.Stream`, and do not add a callback just to push the
same count into manager state.

Keep fields omitted when zero or irrelevant.

### Readiness

Keep local listener streams excluded from `/ready` for now. A running listener
means the local fan-out endpoint is available, not that a remote broadcast path
is confirmed.

The UI can still show listener health:

- listener bound;
- encoder running;
- client count;
- retry/error state.

### UI Behavior

For listener streams, show fan-out/listener state and encoder state separately.
Client connect/disconnect should only update the client count; it should not
look like a stream restart.

Suggested stream card fields:

```text
Status:   Listening / Error / Stopped
Encoder:  Running / Restarting / Failed
Clients:  0 / 1 / n
URL:      srt://<host>:<port>?mode=caller
```

UI requirements:

- Show `client_count` prominently on the stream card.
- Show `encoder_running` as listener-specific process health.
- Derive the label `Encoder: Running / Restarting / Failed` from
  `encoder_running` plus the existing stream process/retry/error state. Do not
  add a separate encoder-state enum in v1.
- Do not use `stable` for listener streams in v1.
- Show the SRT URL with a copy button. Use the configured listener bind host
  when it is a specific address. For wildcard binds (`0.0.0.0` or `::`), use
  `window.location.hostname` as the displayed host, because the server does not
  know which external address the operator wants clients to use.
- When the encoder is retrying while fan-out remains bound, show text such as
  "Listener open, encoder restarting".
- Do not show per-client IP addresses, per-client buffers, or drop counters in
  v1.
- Do not write event-log entries for every client connect/disconnect unless
  there is a clear operational need later.

### Events

Reuse existing stream events:

- `stream_started`: listener fan-out started.
- `stream_error`: encoder process failed or fan-out failed.
- `stream_retry`: encoder retry scheduled.
- `stream_stopped`: listener fan-out stopped intentionally.

Do not emit `stream_stable` for listener streams unless policy changes.

## Configuration Behavior

No new config keys are required for the first implementation.

Defaults:

- listener bind behavior remains unchanged;
- listener duplicate bind validation remains unchanged;
- default listener codec remains MP3;
- default relay latency is 300 ms;
- default max clients should be a conservative constant, for example 16.

Existing configs with specific listener bind addresses remain valid. Interface
restriction should still normally be handled with firewall rules, but preserving
the current validation avoids a breaking config-load change.

Future optional config:

- `max_clients`;
- explicit listener latency.

Do not add these until the core fan-out path is proven.

## Dependency Work

Add:

```bash
go get github.com/datarhei/gosrt
go mod tidy
```

Then run:

```bash
go test ./...
govulncheck ./...
```

Dependency notes:

- GoSRT is pure Go and should not affect Raspberry Pi cross-compilation.
- Upstream currently shows MIT license; confirm before merge.
- FFmpeg SRT support is not required for listener streams after this change.
- FFmpeg SRT support is still required for caller streams.

## Test Plan

### Unit Tests

`internal/srtfanout`:

- all accepted connections are subscribers regardless of Stream ID.
- password-required subscribers reject unencrypted requests.
- subscriber max-client limit rejects subscribers when the active count is at or
  above the limit.
- slow subscriber queue does not block `Server.Write`.
- full subscriber queue drops stale queued chunks and keeps the newest chunk.
- subscriber queue enforces the configured chunk-count limit.
- `ClientCount()` increments and decrements.
- `Shutdown` closes active subscriber connections and waits for subscriber
  writer goroutines.
- `Latency` config uses `300 * time.Millisecond`.

`internal/streaming`:

- listener FFmpeg args write to `pipe:1`, not SRT.
- MPEG-TS listener args include explicit PAT/PMT cadence, for example
  `-pat_period 0.1`.
- caller FFmpeg args remain unchanged.
- FFmpeg SRT capability gating is mode-aware:
  - caller streams require FFmpeg SRT support;
  - listener streams do not.
- listener encoder exits use failure/backoff path, not relisten path.
- old listener relisten constants and tests are removed or updated.
- `Start` rejects duplicate active starts.
- encoder retry uses the encoder-only restart entrypoint, not `Manager.Start`.
- `WriteAudio` drops or skips while no encoder run is connected.
- encoder run replacement has no data race on `result`, `audioCh`, stdout
  reader, or writer state.
- stdout reader forwards each read immediately and does not wait for large
  buffers to fill.
- stdout reader drains until EOF/error and is coordinated with process `Wait`.
- `Stop` closes encoder run, fan-out, and goroutines in deterministic order.
- placeholder cleanup handles fan-out start failure.
- placeholder cleanup handles FFmpeg encoder start failure after fan-out start.

`internal/types` / config:

- existing listener validation remains intact.
- password length remains compatible with GoSRT.

### Integration Tests

Use GoSRT only:

- start fan-out;
- connect two subscribers;
- call `Server.Write` with test bytes;
- verify both subscribers receive the bytes.

Use FFmpeg when available:

- start fan-out;
- start FFmpeg with listener-pipe args and a generated PCM input;
- connect two `ffplay` or GoSRT test subscribers;
- verify both receive audio bytes.
- for Opus and PCM listener outputs, connect a second subscriber after the
  stream has already been running and measure time-to-audio. It should be
  bounded by the explicit PAT/PMT cadence plus normal client/SRT latency.

Mark FFmpeg-dependent tests with skips when FFmpeg is not available.

### Manual Verification

On a development machine or Raspberry Pi:

1. Configure one listener stream on port 9000.
2. Start encoder.
3. Connect two clients at the same time:

   ```bash
   ffplay "srt://<encoder-ip>:9000?mode=caller"
   ffplay "srt://<encoder-ip>:9000?mode=caller"
   ```

4. Confirm both receive audio.
5. If an existing config uses a specific listener bind host, confirm it still
   loads and binds as before.
6. For Opus and PCM listener outputs, start a new client after the stream is
   already running and confirm playback starts promptly.
7. Run with an FFmpeg build that lacks SRT support if available:
   - listener streams should start;
   - caller streams should still fail with the existing SRT unsupported error.
8. Kill the FFmpeg encoder process and confirm:
   - the SRT listener remains bound until max retries are exhausted;
   - retry uses backoff, not the old 500 ms relisten loop;
   - max retries are honored.
9. Stop encoder and confirm the UDP port is released.

## Implementation Sequence

### Phase 1: GoSRT Fan-Out Package

- Add dependency.
- Add `internal/srtfanout`.
- Treat every accepted connection as a subscriber; stream IDs are advisory.
- Implement subscriber password handling.
- Implement simple max-client tracking.
- Implement bounded per-subscriber queues.
- Enforce the subscriber queue chunk-count limit.
- Implement `srt.Server` startup/shutdown wrapper.
- Add unit tests.

### Phase 2: FFmpeg Pipe Args And Process Support

- Split FFmpeg arg builders into caller and listener-pipe paths.
- Keep caller output URL generation unchanged.
- Generate listener output args with `pipe:1`.
- Preserve current listener codec/container choices.
- Set explicit MPEG-TS PAT/PMT cadence for Opus and PCM listener outputs.
- Extend the FFmpeg wrapper so listener runs can read stdout.
- Read stdout in modest chunks and forward each read immediately.
- Add tests for generated args.

### Phase 3: Streaming Manager Rewrite For Listeners

- Split `Manager.Start` internals by stream mode.
- Extend `Stream` with fan-out and encoder-run state.
- Add per-run FFmpeg process/channel/goroutine ownership.
- Add listener-specific monitor path with encoder-only restart.
- Remove old relisten semantics for fan-out-backed listeners.
- Ensure encoder failures use backoff and max retries.
- Ensure `WriteAudio` gates on encoder run liveness, not only fan-out state.
- Ensure fan-out shutdown is awaited before map removal.
- Add manager tests around failure, retry, stop, and placeholder cleanup.

### Phase 4: Status, UI, Docs

- Extend `ProcessStatus`.
- Surface listener `encoder_running` and `client_count`.
- Derive listener copy URL host from configured bind host or
  `window.location.hostname` for wildcard binds.
- Keep `/ready` policy unchanged.
- Update README listener section.
- Update event docs if event wording changes.

### Phase 5: Full Verification

- Run `go test ./...`.
- Run `govulncheck ./...`.
- Run local manual multi-client tests.
- Run on Raspberry Pi or equivalent Linux target before release.

## Risks And Mitigations

| Risk | Mitigation |
| --- | --- |
| Slow subscriber blocks the encoder | Per-subscriber bounded queues; `Server.Write` never blocks on clients. |
| Slow subscriber hears gaps | Keep newest chunks and drop stale queued chunks to stay near live edge. |
| FFmpeg stdout blocks | Always drain stdout, even when there are no subscribers. |
| Stdout reader adds latency | Use modest read buffers and forward each successful read immediately. |
| Late MPEG-TS subscribers wait for tables | Set explicit PAT/PMT cadence for MPEG-TS listener outputs and test time-to-audio for late joins. |
| Infinite fast encoder restart loop | Remove listener relisten path; use normal backoff and max retries. |
| UDP port not released before restart | Fan-out `Shutdown` must be followed by `Wait`; test stop/start. |
| Writer/process data race across encoder runs | Give every encoder run its own `result`, `audioCh`, stdout reader, and writer; guard run replacement with `encoderMu`. |
| FFmpeg SRT support absent | Listener streams no longer require it; caller streams still use existing `srtAvailable` gate. |
| Dependency mismatch or vulnerability | Run `go mod tidy`, `go test`, `govulncheck`, and license review. |
| Manager lock regression | Preserve placeholder ownership pattern and avoid holding `m.mu` across process I/O, SRT writes, or shutdown waits. |

## Alternative: GoSRT PubSub With Loopback Publisher

The rejected alternative is:

```text
PCM distributor -> FFmpeg -> loopback SRT publisher -> GoSRT PubSub -> subscribers
```

Benefits:

- less custom fan-out code;
- GoSRT `PubSub` already skips slow subscribers;
- one-publisher/many-subscribers behavior is already implemented upstream.

Costs:

- adds an internal SRT receiver buffer;
- still requires FFmpeg SRT support for listener streams;
- requires a local publisher lifecycle in addition to the external listener;
- makes listener latency harder to reason about.

Keep this as a fallback design only if custom byte fan-out proves too fragile.

## Merge Criteria

Do not merge until all of these are true:

- Two clients can listen to one listener stream concurrently.
- Listener FFmpeg output uses stdout pipe, not SRT.
- Subscriber queues enforce a bounded chunk limit.
- MPEG-TS listener outputs set explicit PAT/PMT cadence.
- Late-joining Opus/PCM clients start playback promptly in manual verification.
- Listener streams can start without FFmpeg SRT support.
- Caller streams still require FFmpeg SRT support.
- UI copy URL uses a reachable host for wildcard binds.
- Listener encoder failures use backoff and max retries.
- Old FFmpeg listener relisten logic is removed or proven unreachable.
- Encoder retry does not call `Manager.Start` and does not rebind fan-out.
- Encoder run replacement has no `result`/`audioCh`/stdout data race.
- Fan-out shutdown releases the UDP port reliably.
- Fan-out shutdown closes existing subscriber connections and waits for
  subscriber writer goroutines.
- Existing listener bind configs remain valid.
- Password-protected listener streams reject unencrypted subscribers.
- Caller streams still pass existing tests.
- `go test ./...` passes.
- Dependency/license/vulnerability checks are complete.
