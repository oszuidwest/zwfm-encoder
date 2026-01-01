# Architecture Guide

This document explains the key architectural patterns and Go idioms used in this codebase.

## Package Structure

```
internal/
├── audio/       # Audio capture, level metering, silence detection
├── config/      # Configuration management (JSON file)
├── encoder/     # Main orchestration: audio capture → distribution → outputs
├── ffmpeg/      # Shared FFmpeg process utilities
├── notify/      # Silence notifications (webhook, email, log)
├── output/      # Output stream management (SRT destinations)
├── recording/   # File recording with S3 upload
├── server/      # WebSocket command handlers
├── types/       # Shared types and constants
└── util/        # Utilities (backoff, signals, validation)
```

**Why this structure?**
- Each package has a single responsibility
- `internal/` prevents external imports (Go convention)
- Shared types live in `types/` to avoid circular imports

---

## Key Patterns

### 1. Mutex Protection (sync.RWMutex)

Most structs that are accessed from multiple goroutines use `sync.RWMutex`:

```go
type Manager struct {
    mu        sync.RWMutex
    processes map[string]*Process
}
```

**Pattern:**
- `RLock()` for reads (multiple readers allowed)
- `Lock()` for writes (exclusive access)
- Always unlock with `defer` or explicit `Unlock()`

**Example from `output/manager.go`:**
```go
func (m *Manager) WriteAudio(outputID string, data []byte) error {
    m.mu.RLock()                    // Read lock - others can read too
    proc, exists := m.processes[outputID]
    m.mu.RUnlock()                  // Release before slow I/O

    // Write outside lock to prevent blocking other operations
    _, err := stdin.Write(data)
    return err
}
```

### 2. Protecting I/O with Separate Mutex

When you need to do I/O (like writing to stdin) but also protect shared state, use two mutexes:

```go
type GenericRecorder struct {
    mu      sync.RWMutex  // Protects state (config, running, etc.)
    stdinMu sync.Mutex    // Protects stdin writes specifically
}
```

**Why?** Holding `mu` during slow I/O would block all other operations. The separate `stdinMu` only blocks concurrent writes to stdin.

### 3. Channel Communication

Channels are used for:
- **Stop signals**: `stopCh chan struct{}`
- **Work queues**: `uploadQueue chan uploadRequest`
- **Event notification**: `done chan struct{}`

**Stop channel pattern:**
```go
func (r *GenericRecorder) uploadWorker() {
    for {
        select {
        case <-r.uploadStopCh:   // Check for stop signal
            return
        case req := <-r.uploadQueue:
            r.uploadFile(req)
        }
    }
}
```

### 4. Context for Cancellation

Use `context.Context` for operations that should be cancellable:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()

_, err = client.PutObject(ctx, &s3.PutObjectInput{...})
```

### 5. Exponential Backoff

Failed operations use exponential backoff to avoid hammering failing services:

```go
backoff := util.NewBackoff(3*time.Second, 60*time.Second)

// After each failure:
delay := backoff.Next()  // Returns 3s, 6s, 12s, 24s, 48s, 60s, 60s...

// After success:
backoff.Reset()  // Back to 3s
```

---

## Audio Pipeline

```
┌─────────────┐    ┌─────────────┐    ┌──────────────┐
│   arecord   │───▶│ Distributor │───▶│ Output 1     │──▶ SRT Server
│  (capture)  │    │             │    └──────────────┘
└─────────────┘    │  • Levels   │    ┌──────────────┐
                   │  • Silence  │───▶│ Output 2     │──▶ SRT Server
                   │  • Peaks    │    └──────────────┘
                   └─────────────┘    ┌──────────────┐
                         │           ▶│ Recorder     │──▶ S3/Local
                         └───────────▶└──────────────┘
```

1. **Capture**: Platform-specific (arecord on Linux, AVFoundation on macOS)
2. **Distributor**: Calculates audio levels, detects silence, fans out PCM data
3. **Outputs**: Each output has its own FFmpeg process encoding to SRT
4. **Recording**: Separate FFmpeg processes for file recording

---

## Concurrency Model

### Goroutines
- `runDistributor()`: Main audio distribution loop
- `MonitorAndRetry()`: One per output, handles restarts
- `uploadWorker()`: One per recorder, handles S3 uploads
- `run()`: Version checker (periodic GitHub checks)

### Shutdown Order
1. Signal received (SIGTERM/SIGINT)
2. Stop version checker
3. Shutdown HTTP server
4. Stop encoder (stops audio capture)
5. Stop outputs and recordings

---

## Error Handling

### Wrapping Errors
Always add context when returning errors:
```go
if err != nil {
    return fmt.Errorf("create S3 client: %w", err)
}
```

### Sentinel Errors
Defined errors for known conditions:
```go
var ErrNoAudioInput = errors.New("no audio input configured")
```

Check with `errors.Is()`:
```go
if errors.Is(err, ErrNoAudioInput) {
    // Handle specifically
}
```

---

## Configuration

Configuration is stored in JSON and protected by mutex:

```go
type Config struct {
    mu   sync.RWMutex
    path string
    data ConfigData
}

// Thread-safe read
func (c *Config) Snapshot() ConfigData {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.data  // Returns a copy
}
```

---

## Common Pitfalls to Avoid

1. **Holding locks during I/O**: Release mutex before slow operations
2. **Goroutine leaks**: Always have a way to stop goroutines (stop channel)
3. **Race conditions**: Use `-race` flag during development: `go run -race .`
4. **Blocking channels**: Use buffered channels or `select` with `default`

---

## Testing Concurrency

Run with race detector during development:
```bash
go run -race .
go test -race ./...
```

This catches data races at runtime.
