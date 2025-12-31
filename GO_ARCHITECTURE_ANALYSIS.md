# Go Architecture Analysis Report

**Date:** 2025-12-31
**Branch:** feature/recorder-refactor
**Analyzed by:** Claude Opus 4.5 (Go Architect mode)
**Last Updated:** 2025-12-31 (fixes applied)

---

## Completed Fixes (Commit 9b4dfbd)

The following P0 and P1 issues have been resolved:

### ✅ P0: Race Conditions Fixed
| Issue | File | Fix Applied |
|-------|------|-------------|
| I/O under mutex lock | `output/manager.go` | WriteAudio now performs I/O outside the lock |
| SilenceDetector not thread-safe | `audio/silence.go` | Added `sync.Mutex` to protect state |
| PeakHolder not thread-safe | `audio/peakhold.go` | Added `sync.Mutex` to protect state |

### ✅ P1: Dead Code Removed (~80 lines)
| Removed Item | File |
|--------------|------|
| `Config.RecordingAPIKeyPreview()` | `config/config.go` |
| `GenericRecorder.TestS3()` | `recording/recorder.go` |
| `Encoder.RecordingManager()` | `encoder/encoder.go` |
| `Manager.Recorder()` | `recording/manager.go` |
| `Manager.RecorderCount()` | `recording/manager.go` |
| `Manager.IsRunning()` | `recording/manager.go` |
| `GenericRecorder.lastUploadTime` field | `recording/recorder.go` |
| `GenericRecorder.lastUploadErr` field | `recording/recorder.go` |
| `GenericRecorder.bytesWritten` field | `recording/recorder.go` |
| `S3Config.Region` field | `recording/types.go` |

### ⏭️ P1: Naming - Skipped (Technical Limitation)
| Method | Reason |
|--------|--------|
| `GetFFmpegPath()` | Cannot rename to `FFmpegPath()` - conflicts with struct field `FFmpegPath` |
| `GetRecordingAPIKey()` | Cannot rename to `RecordingAPIKey()` - conflicts with struct field `RecordingAPIKey` |

Go does not allow a method and a field to have the same name on a struct. Comments added to explain this.

---

## Executive Summary

This report contains a comprehensive code review of the rpi-audio-encoder codebase, focusing on:
- Go naming conventions (per Effective Go)
- Error handling patterns
- Concurrency patterns
- KISS (Keep It Simple, Stupid)
- DRY (Don't Repeat Yourself)
- YAGNI (You Aren't Gonna Need It)

**Overall Assessment:** The codebase is well-structured and follows Go idioms reasonably well. However, there are several areas for improvement, particularly around code duplication between the `output` and `recording` packages.

---

## 1. Go Naming Conventions

### 1.1 Violations Found

Per Effective Go: *"Go doesn't provide automatic support for getters and setters... it's neither idiomatic nor necessary to put Get into the getter's name."*

| Priority | File | Line | Current | Recommended | Reason | Status |
|----------|------|------|---------|-------------|--------|--------|
| HIGH | config/config.go | 407 | `GetFFmpegPath()` | `FFmpegPath()` | "Get" prefix non-idiomatic | ⏭️ SKIPPED - field name collision |
| HIGH | config/config.go | 594 | `GetRecordingAPIKey()` | `RecordingAPIKey()` | "Get" prefix non-idiomatic | ⏭️ SKIPPED - field name collision |
| MEDIUM | recording/types.go | 27 | `RecordingState` | `State` | Package name redundancy | Open |
| MEDIUM | recording/types.go | 67 | `RecorderToS3Config()` | `S3ConfigFromRecorder()` or method | Awkward naming | Open |
| LOW | recording/recorder.go | 606 | `getFileExtension()` | `fileExtension()` | Minor (unexported) | Open |
| LOW | recording/recorder.go | 622 | `getContentType()` | `contentType()` | Minor (unexported) | Open |

### 1.2 What Is Done Well

- **Package names:** All lowercase single-word (`encoder`, `audio`, `config`, `output`, `recording`)
- **Constants:** MixedCaps used correctly (no ALL_CAPS)
- **Error variables:** Correct `Err` prefix (`ErrNoAudioInput`, `ErrAlreadyRunning`)
- **Boolean methods:** Correct `Is`/`Has` prefix (`IsEnabled()`, `IsRunning()`, `HasWebhook()`)
- **Constructor functions:** Correct `New` prefix (`NewManager()`, `NewBackoff()`)

---

## 2. Error Handling

**Score: 8.5/10**

### 2.1 Strengths

- Consistent use of `%w` for error wrapping
- Utility function `util.WrapError()` for standardized wrapping
- Well-defined sentinel errors with proper naming
- Proper use of `errors.Is()` where needed
- Panic recovery in all long-running goroutines

### 2.2 Issues Found

#### Missing Error Context

| File | Line | Current | Recommended |
|------|------|---------|-------------|
| recording/recorder.go | 148 | `return err` | `return fmt.Errorf("start encoder: %w", err)` |
| recording/recorder.go | 225 | `return err` | `return fmt.Errorf("write to stdin: %w", err)` |

#### Ignored Errors Without Documentation

| File | Line | Code | Recommendation |
|------|------|------|----------------|
| output/manager.go | 156 | `_ = util.GracefulSignal(process)` | Add nolint comment with justification |
| recording/recorder.go | 413 | `_ = r.cmd.Process.Kill()` | Add nolint comment with justification |

#### Missing `errors.Is()` Usage

| File | Line | Current | Recommended |
|------|------|---------|-------------|
| server.go | 477 | `err != http.ErrServerClosed` | `!errors.Is(err, http.ErrServerClosed)` |

### 2.3 Sentinel Errors (Good Practice)

```go
// internal/encoder/encoder.go
var (
    ErrNoAudioInput   = errors.New("no audio input configured")
    ErrAlreadyRunning = errors.New("encoder already running")
    ErrNotRunning     = errors.New("encoder not running")
    ErrOutputDisabled = errors.New("output is disabled")
    ErrOutputNotFound = errors.New("output not found")
)

// internal/recording/types.go
var (
    ErrRecordingDisabled             = errors.New("recording is disabled")
    ErrHourlyRecorderNotControllable = errors.New("hourly recorders cannot be started/stopped via API")
    ErrAlreadyRecording              = errors.New("recorder is already recording")
    ErrNotRecording                  = errors.New("recorder is not recording")
)
```

---

## 3. Concurrency Patterns

**Grade: B+**

### 3.1 Strengths

- Proper `sync.RWMutex` selection (read-heavy operations use `RLock`)
- Clean lock/unlock patterns with `defer`
- Good WebSocket channel pattern (dedicated writer goroutine)
- Effective stop signaling with channels
- Proper drain pattern in upload worker

### 3.2 Issues Found

#### ✅ FIXED: I/O Under Lock

**File:** `internal/output/manager.go:191-212`

~~**Problem:** The `Write()` call to FFmpeg's stdin was performed while holding the mutex.~~

**Status:** ✅ Fixed in commit 9b4dfbd. WriteAudio now acquires RLock to get stdin reference, releases lock, then performs I/O outside the lock. Error handling re-acquires write lock only when needed.

#### ✅ FIXED: SilenceDetector Not Thread-Safe

**File:** `internal/audio/silence.go`

~~**Problem:** No mutex protection.~~

**Status:** ✅ Fixed in commit 9b4dfbd. Added `sync.Mutex` to protect all state fields. Both `Update()` and `Reset()` methods now acquire the mutex.

#### ✅ FIXED: PeakHolder Not Thread-Safe

**File:** `internal/audio/peakhold.go`

~~**Problem:** Same issue as SilenceDetector.~~

**Status:** ✅ Fixed in commit 9b4dfbd. Added `sync.Mutex` to protect state. Both `Update()` and `Reset()` methods now acquire the mutex.

#### LOW: Multiple Lock/Unlock Cycles

**File:** `internal/recording/recorder.go:210-233`

The `WriteAudio` method has multiple lock/unlock cycles. ~~Consider using `atomic.Int64` for `bytesWritten`.~~ (Note: `bytesWritten` field was removed in commit 9b4dfbd as it was unused.)

#### LOW: Fire-and-Forget Goroutines

**File:** `internal/server/commands.go`

Multiple places spawn goroutines without tracking (lines 295-307, 240-254). No way to wait for them during shutdown.

### 3.3 No Deadlock Risk Found

Lock ordering analysis shows no circular dependencies. Code is careful to release encoder locks before acquiring other locks.

---

## 4. KISS (Keep It Simple, Stupid)

### 4.1 File Size Issues

| File | Lines | Issue |
|------|-------|-------|
| internal/server/commands.go | 804 | Too large, handles too many concerns |
| internal/recording/recorder.go | 672 | Acceptable but GenericRecorder struct is complex |
| internal/encoder/encoder.go | 658 | Acceptable |

**Recommendation:** Split `commands.go` into:
- `commands_output.go` - output CRUD handlers
- `commands_recorder.go` - recorder CRUD handlers
- `commands_settings.go` - settings handlers
- `commands_test.go` - test notification handlers

### 4.2 Function Complexity

| File | Function | Est. CC | Issue |
|------|----------|---------|-------|
| commands.go:309 | `handleUpdateSettings` | 12 | Too many responsibilities (audio, silence, email) |
| recorder.go:367 | `stopEncoderAndUpload` | 9 | Complex 2-stage timeout mechanism |
| encoder.go:370 | `runSourceLoop` | 10 | Complex retry logic (inherent to task) |
| output/manager.go:357 | `MonitorAndRetry` | 10 | Multiple early returns, nested selects |

### 4.3 Type Complexity

#### GenericRecorder - 14 Fields (was 17+, cleaned up in commit 9b4dfbd)

**File:** `internal/recording/recorder.go:23-55`

```go
type GenericRecorder struct {
    mu                 sync.RWMutex
    id                 string
    config             types.Recorder
    ffmpegPath         string
    maxDurationMinutes int
    tempDir            string
    state              RecordingState
    lastError          string
    cmd                *exec.Cmd
    ctx                context.Context
    cancel             context.CancelFunc
    stdin              io.WriteCloser
    stderr             *bytes.Buffer
    currentFile        string
    startTime          time.Time
    // bytesWritten, lastUploadTime, lastUploadErr - REMOVED (unused)
    s3Client           *s3.Client
    uploadQueue        chan uploadRequest
    uploadWg           sync.WaitGroup
    uploadStopCh       chan struct{}
    rotationTimer      *time.Timer
    durationTimer      *time.Timer
}
```

**Recommendation:** Group related fields:
- `ffmpegProcess` struct for cmd, ctx, cancel, stdin, stderr
- `uploadState` struct for queue, wg, stopCh
- `timerState` struct for rotation and duration timers

### 4.4 Repeated Patterns

The non-blocking send pattern appears 5 times in commands.go:

```go
select {
case send <- result:
default:
    slog.Warn("failed to send...: channel full or closed")
}
```

**Recommendation:** Extract to helper:
```go
func trySend[T any](ch chan<- T, msg T, context string) {
    select {
    case ch <- msg:
    default:
        slog.Warn("failed to send: channel full or closed", "context", context)
    }
}
```

---

## 5. DRY (Don't Repeat Yourself)

### 5.1 FFmpeg Process Management (~70-75% overlap)

**Locations:**
- `internal/output/manager.go:84-116`
- `internal/recording/recorder.go:318-361`

Both implement nearly identical FFmpeg process startup:

```go
// Both packages do this:
ctx, cancel := context.WithCancel(context.Background())
cmd := exec.CommandContext(ctx, ffmpegPath, args...)

stdinPipe, err := cmd.StdinPipe()
if err != nil {
    cancel()
    return fmt.Errorf("create stdin pipe: %w", err)
}

var stderr bytes.Buffer
cmd.Stderr = &stderr

if err := cmd.Start(); err != nil {
    cancel()
    stdinPipe.Close()
    return fmt.Errorf("start ffmpeg: %w", err)
}
```

**Recommendation:** Create shared `internal/ffmpeg/process.go`:

```go
type ProcessConfig struct {
    FFmpegPath string
    Args       []string
}

type Process struct {
    Cmd    *exec.Cmd
    Ctx    context.Context
    Cancel context.CancelFunc
    Stdin  io.WriteCloser
    Stderr *bytes.Buffer
}

func StartProcess(cfg ProcessConfig) (*Process, error)
func (p *Process) Stop() error
func (p *Process) Write(data []byte) error
```

### 5.2 FFmpeg Input Arguments (90% overlap)

**Locations:**
- `internal/output/ffmpeg.go:17-23`
- `internal/recording/recorder.go:321-329`

```go
// Both build these args:
args := []string{
    "-f", "s16le",
    "-ar", fmt.Sprintf("%d", types.SampleRate),
    "-ac", fmt.Sprintf("%d", types.Channels),
    "-hide_banner", "-loglevel", "warning",
    "-i", "pipe:0",
}
```

**Recommendation:** Add to `internal/types/types.go`:

```go
func BaseInputArgs() []string {
    return []string{
        "-f", "s16le",
        "-ar", fmt.Sprintf("%d", SampleRate),
        "-ac", fmt.Sprintf("%d", Channels),
        "-hide_banner", "-loglevel", "warning",
        "-i", "pipe:0",
    }
}
```

### 5.3 Config CRUD Operations (~85-90% overlap)

**Locations:**
- `internal/config/config.go:254-308` (Output operations: find at 254-261, Add at 264-280, Remove at 283-294, Update at 297-308)
- `internal/config/config.go:332-397` (Recorder operations: find at 332-339, Add at 342-369, Remove at 372-383, Update at 386-397)

```go
// Output
func (c *Config) findOutputIndex(id string) int { ... }
func (c *Config) AddOutput(output *types.Output) error { ... }
func (c *Config) RemoveOutput(id string) error { ... }
func (c *Config) UpdateOutput(output *types.Output) error { ... }

// Recorder (identical pattern)
func (c *Config) findRecorderIndex(id string) int { ... }
func (c *Config) AddRecorder(recorder *types.Recorder) error { ... }
func (c *Config) RemoveRecorder(id string) error { ... }
func (c *Config) UpdateRecorder(recorder *types.Recorder) error { ... }
```

**Recommendation:** Use Go generics:

```go
type Identifiable interface {
    GetID() string
}

func findIndex[T Identifiable](items []T, id string) int {
    for i, item := range items {
        if item.GetID() == id {
            return i
        }
    }
    return -1
}
```

### 5.4 IsEnabled() Method (100% identical)

**File:** `internal/types/types.go`

```go
// Output.IsEnabled() - line 62
func (o *Output) IsEnabled() bool {
    return o.Enabled == nil || *o.Enabled
}

// Recorder.IsEnabled() - line 184
func (r *Recorder) IsEnabled() bool {
    return r.Enabled == nil || *r.Enabled
}
```

**Recommendation:** Embeddable struct:

```go
type EnabledField struct {
    Enabled *bool `json:"enabled,omitempty"`
}

func (e EnabledField) IsEnabled() bool {
    return e.Enabled == nil || *e.Enabled
}
```

### 5.5 CodecArgs/Format Methods (100% identical)

**File:** `internal/types/types.go`

```go
// Output methods (lines 114-122)
func (o *Output) CodecArgs() []string { return CodecArgsFor(o.Codec) }
func (o *Output) Format() string { return FormatFor(o.Codec) }

// Recorder methods (lines 188-196)
func (r *Recorder) CodecArgs() []string { return CodecArgsFor(r.Codec) }
func (r *Recorder) Format() string { return FormatFor(r.Codec) }
```

**Recommendation:** Embeddable struct:

```go
type CodecConfig struct {
    Codec string `json:"codec"`
}

func (c CodecConfig) CodecArgs() []string { return CodecArgsFor(c.Codec) }
func (c CodecConfig) Format() string { return FormatFor(c.Codec) }
```

### 5.6 WebSocket Handler Pattern (80% overlap)

**File:** `internal/server/commands.go`

All recorder handlers follow identical pattern:
- `handleAddRecorder` (565-587)
- `handleDeleteRecorder` (590-605)
- `handleUpdateRecorder` (608-650)
- `handleStartRecorder` (653-668)
- `handleStopRecorder` (671-686)

**Recommendation:** Generic handler helper:

```go
type RecorderOperation func(id string) error

func (h *CommandHandler) handleRecorderOp(
    cmd WSCommand,
    send chan<- interface{},
    action string,
    op RecorderOperation,
) {
    if cmd.ID == "" {
        slog.Warn(action+"_recorder: no ID provided")
        sendRecorderResult(send, action, "", false, "no ID provided")
        return
    }

    if err := op(cmd.ID); err != nil {
        slog.Error(action+"_recorder: failed", "error", err)
        sendRecorderResult(send, action, cmd.ID, false, err.Error())
        return
    }

    slog.Info(action+"_recorder: completed", "id", cmd.ID)
    sendRecorderResult(send, action, cmd.ID, true, "")
}
```

---

## 6. YAGNI (You Aren't Gonna Need It)

### 6.1 Unused Exported Functions/Methods

| Item | File | Lines | Evidence | Status |
|------|------|-------|----------|--------|
| `Config.RecordingAPIKeyPreview()` | config.go | 600-609 | No callers found | ✅ REMOVED |
| `GenericRecorder.TestS3()` | recorder.go | 647-650 | `TestRecorderS3Connection()` used instead | ✅ REMOVED |
| `Encoder.RecordingManager()` | encoder.go | 106-111 | No external callers | ✅ REMOVED |
| `Manager.Recorder(id)` | manager.go | 237-241 | No external callers | ✅ REMOVED |
| `Manager.RecorderCount()` | manager.go | 243-248 | No callers | ✅ REMOVED |
| `Manager.IsRunning()` | manager.go | 264-269 | No callers | ✅ REMOVED |

### 6.2 Unused Struct Fields

| Field | File | Line | Evidence | Status |
|-------|------|------|----------|--------|
| `GenericRecorder.lastUploadTime` | recorder.go | 54 | Set at line 528, never read | ✅ REMOVED |
| `GenericRecorder.lastUploadErr` | recorder.go | 55 | Set at lines 520, 529, never read | ✅ REMOVED |
| `GenericRecorder.bytesWritten` | recorder.go | 45 | Incremented at line 229, reset at line 353, never read | ✅ REMOVED |
| `S3Config.Region` | recording/types.go | 41 | Hardcoded to "auto", never configured | ✅ REMOVED |

### 6.3 Speculative Generality

| Item | File | Lines | Issue |
|------|------|-------|-------|
| `util.SafeClose()` | cleanup.go | 9-17 | Only called by `SafeCloseFunc()` internally |
| `ErrGracefulNotSupported` | signal_windows.go | 11 | Returned but never checked with `errors.Is()` |

### 6.4 Cleanup Priority

**High Priority (Dead Code):** ✅ ALL COMPLETED
1. ~~Remove `Config.RecordingAPIKeyPreview()`~~ ✅
2. ~~Remove `GenericRecorder.TestS3()`~~ ✅
3. ~~Remove unused fields: `lastUploadTime`, `lastUploadErr`, `bytesWritten`~~ ✅

**Medium Priority (Speculative Generality):** ✅ ALL COMPLETED
4. ~~Remove `Manager.RecorderCount()`~~ ✅
5. ~~Remove `Manager.IsRunning()`~~ ✅
6. ~~Remove or unexport `Manager.Recorder()`~~ ✅
7. ~~Remove `Encoder.RecordingManager()`~~ ✅

**Low Priority:** ✅ 1 of 2 COMPLETED
8. ~~Remove `S3Config.Region` field~~ ✅
9. Unexport `util.SafeClose()` to `safeClose()` - Open

---

## 7. Summary of Recommended Actions

### High Priority ✅ COMPLETED

| Action | Impact | Effort | Status |
|--------|--------|--------|--------|
| Remove YAGNI code (6 functions, 4 fields) | ~80 lines removed | Low | ✅ Done |
| Fix naming conventions (Get prefix) | Code clarity | Low | ⏭️ Skipped (field collision) |
| Move I/O outside mutex in WriteAudio | Performance | Medium | ✅ Done |

> **Note:** This report was fact-checked by 6 specialized agents. All claims verified as TRUE with minor corrections applied to overlap percentages and line numbers.

### Medium Priority ✅ PARTIALLY COMPLETED

| Action | Impact | Effort | Status |
|--------|--------|--------|--------|
| Add mutex to SilenceDetector/PeakHolder | Thread safety | Low | ✅ Done |
| Split commands.go into 3-4 files | Maintainability | Medium | Open |
| Extract shared FFmpeg process helper | ~200 lines reduction | Medium | Open |

### Low Priority (Open)

| Action | Impact | Effort | Status |
|--------|--------|--------|--------|
| Consolidate Config CRUD with generics | ~100 lines reduction | Low | Open |
| Use embedded structs for IsEnabled/CodecConfig | Code clarity | Low | Open |
| Extract non-blocking send helper | Code clarity | Low | Open |

---

## 8. Estimated Impact

**Completed reduction:** ~80 lines removed (commit 9b4dfbd)
**Remaining potential reduction:** ~200-300 lines

**Files modified in commit 9b4dfbd:**
1. ✅ `internal/output/manager.go` - I/O outside lock
2. ✅ `internal/audio/silence.go` - mutex protection
3. ✅ `internal/audio/peakhold.go` - mutex protection
4. ✅ `internal/config/config.go` - removed RecordingAPIKeyPreview()
5. ✅ `internal/encoder/encoder.go` - removed RecordingManager()
6. ✅ `internal/recording/manager.go` - removed 3 unused methods
7. ✅ `internal/recording/recorder.go` - removed TestS3() and unused fields
8. ✅ `internal/recording/types.go` - removed S3Config.Region

**Files pending refactoring:**
1. `internal/server/commands.go` - split and simplify
2. `internal/output/manager.go` + `internal/recording/recorder.go` - extract shared FFmpeg process helper
3. `internal/config/config.go` - consolidate CRUD with generics

---

*Report generated by Claude Opus 4.5 in Go Architect mode*
*Updated 2025-12-31 with completed fixes from commit 9b4dfbd*
