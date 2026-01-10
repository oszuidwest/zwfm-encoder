# Error Handling Architecture

Dit document beschrijft het voorgestelde error handling systeem voor de ZuidWest FM Encoder.

## Overzicht

![Simple Diagram](error-handling-simple.png)

## Ontwerpprincipes

1. **Go-idiomatisch** - Geen Java-achtige exception hierarchieën
2. **Juiste abstractieniveau** - Niet over-engineered voor een embedded audio encoder
3. **Observability-first** - Errors zijn data, niet alleen strings
4. **Fail-fast waar mogelijk** - Validatie vooraf, niet tijdens runtime

## Package Structuur

```
internal/
├── errors/
│   ├── errors.go       # Core error type met codes
│   └── ffmpeg.go       # FFmpeg stderr parsing
└── resilience/
    ├── retry.go        # Unified retry state
    └── backoff.go      # Exponential backoff (bestaand)
```

## Gedetailleerd Diagram

![Detailed Diagram](error-handling-detailed.png)

---

## 1. Core Error Type

### `internal/errors/errors.go`

```go
package errors

import (
    "errors"
    "fmt"
)

// ErrorCode voor programmatische handling
type ErrorCode string

const (
    // Validation errors (4xx - client can fix)
    ErrCodeValidation    ErrorCode = "VALIDATION_ERROR"
    ErrCodeNotFound      ErrorCode = "NOT_FOUND"
    ErrCodeInvalidState  ErrorCode = "INVALID_STATE"

    // Infrastructure errors (5xx - transient)
    ErrCodeConnection    ErrorCode = "CONNECTION_ERROR"
    ErrCodeTimeout       ErrorCode = "TIMEOUT"
    ErrCodeProcessCrash  ErrorCode = "PROCESS_CRASH"

    // External service errors
    ErrCodeWebhook       ErrorCode = "WEBHOOK_ERROR"
    ErrCodeEmail         ErrorCode = "EMAIL_ERROR"
    ErrCodeS3            ErrorCode = "S3_ERROR"

    // Fatal errors (niet recoverable)
    ErrCodeNoDevice      ErrorCode = "NO_AUDIO_DEVICE"
    ErrCodeFFmpegMissing ErrorCode = "FFMPEG_NOT_FOUND"
)

// Severity voor logging/alerting
type Severity int

const (
    SeverityDebug Severity = iota
    SeverityInfo
    SeverityWarn
    SeverityError
    SeverityCritical
)

// Error is de centrale error type
type Error struct {
    Code      ErrorCode
    Message   string
    Cause     error
    Component string         // "encoder", "output", "recorder", "notify"
    Retryable bool
    Severity  Severity
    Context   map[string]any // Extra metadata
}

func (e *Error) Error() string {
    if e.Cause != nil {
        return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
    }
    return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
    return e.Cause
}

// Builder pattern voor ergonomische error creatie
func New(code ErrorCode, message string) *Error {
    return &Error{
        Code:      code,
        Message:   message,
        Severity:  SeverityError,
        Retryable: false,
        Context:   make(map[string]any),
    }
}

func (e *Error) WithCause(err error) *Error {
    e.Cause = err
    return e
}

func (e *Error) WithComponent(c string) *Error {
    e.Component = c
    return e
}

func (e *Error) WithRetryable(r bool) *Error {
    e.Retryable = r
    return e
}

func (e *Error) WithSeverity(s Severity) *Error {
    e.Severity = s
    return e
}

func (e *Error) With(key string, value any) *Error {
    e.Context[key] = value
    return e
}
```

### Helper Functies

```go
// Helper functies voor veelvoorkomende errors
func Validation(field, message string) *Error {
    return New(ErrCodeValidation, message).
        WithSeverity(SeverityWarn).
        With("field", field)
}

func NotFound(resource, id string) *Error {
    return New(ErrCodeNotFound, fmt.Sprintf("%s not found: %s", resource, id)).
        WithSeverity(SeverityWarn).
        With("resource", resource).
        With("id", id)
}

func Connection(host string, err error) *Error {
    return New(ErrCodeConnection, fmt.Sprintf("connection to %s failed", host)).
        WithCause(err).
        WithRetryable(true).
        With("host", host)
}

func ProcessCrash(process string, stderr string) *Error {
    return New(ErrCodeProcessCrash, fmt.Sprintf("%s process crashed", process)).
        WithRetryable(true).
        WithComponent(process).
        With("stderr", stderr)
}

// Is checkt of een error een specifieke code heeft
func Is(err error, code ErrorCode) bool {
    var e *Error
    if errors.As(err, &e) {
        return e.Code == code
    }
    return false
}

// IsRetryable checkt of een error retry-waardig is
func IsRetryable(err error) bool {
    var e *Error
    if errors.As(err, &e) {
        return e.Retryable
    }
    return false
}
```

---

## 2. FFmpeg Error Parsing

### `internal/errors/ffmpeg.go`

```go
package errors

import (
    "regexp"
    "strings"
)

// FFmpeg error patterns met hun classificatie
var ffmpegPatterns = []struct {
    Pattern   *regexp.Regexp
    Code      ErrorCode
    Message   string
    Retryable bool
}{
    {
        regexp.MustCompile(`Connection refused`),
        ErrCodeConnection,
        "SRT connection refused",
        true,
    },
    {
        regexp.MustCompile(`Connection timed out`),
        ErrCodeTimeout,
        "SRT connection timeout",
        true,
    },
    {
        regexp.MustCompile(`No such device`),
        ErrCodeNoDevice,
        "Audio device not found",
        false,
    },
    {
        regexp.MustCompile(`Device or resource busy`),
        ErrCodeInvalidState,
        "Audio device busy",
        true,
    },
    {
        regexp.MustCompile(`Invalid data found`),
        ErrCodeProcessCrash,
        "Invalid audio data",
        true,
    },
    {
        regexp.MustCompile(`Broken pipe`),
        ErrCodeConnection,
        "Output connection lost",
        true,
    },
}

// ParseFFmpegError analyseert stderr en retourneert een typed error
func ParseFFmpegError(stderr string, component string) *Error {
    // Check known patterns
    for _, p := range ffmpegPatterns {
        if p.Pattern.MatchString(stderr) {
            return New(p.Code, p.Message).
                WithComponent(component).
                WithRetryable(p.Retryable).
                With("stderr", truncate(stderr, 500))
        }
    }

    // Fallback: extract last meaningful line
    lastLine := extractLastLine(stderr)
    return New(ErrCodeProcessCrash, "FFmpeg error").
        WithComponent(component).
        WithRetryable(true).
        With("detail", lastLine).
        With("stderr", truncate(stderr, 500))
}

func extractLastLine(s string) string {
    lines := strings.Split(strings.TrimSpace(s), "\n")
    for i := len(lines) - 1; i >= 0; i-- {
        line := strings.TrimSpace(lines[i])
        if line != "" && !strings.HasPrefix(line, "frame=") {
            return truncate(line, 200)
        }
    }
    return ""
}

func truncate(s string, max int) string {
    if len(s) <= max {
        return s
    }
    return s[:max] + "..."
}
```

---

## 3. Unified Retry System

### `internal/resilience/retry.go`

```go
package resilience

import (
    "time"

    "encoder/internal/errors"
)

// RetryConfig definieert retry gedrag
type RetryConfig struct {
    MaxAttempts     int
    InitialDelay    time.Duration
    MaxDelay        time.Duration
    BackoffFactor   float64
    JitterFactor    float64
    StableThreshold time.Duration // Reset attempts na stabiele periode
}

// DefaultRetryConfig voor outputs
var DefaultRetryConfig = RetryConfig{
    MaxAttempts:     99,
    InitialDelay:    3 * time.Second,
    MaxDelay:        60 * time.Second,
    BackoffFactor:   2.0,
    JitterFactor:    0.5,
    StableThreshold: 30 * time.Second,
}

// SourceRetryConfig voor audio capture (strenger)
var SourceRetryConfig = RetryConfig{
    MaxAttempts:     10,
    InitialDelay:    3 * time.Second,
    MaxDelay:        60 * time.Second,
    BackoffFactor:   2.0,
    JitterFactor:    0.5,
    StableThreshold: 30 * time.Second,
}

// RetryState houdt de huidige retry status bij
type RetryState struct {
    Attempts    int
    LastAttempt time.Time
    LastSuccess time.Time
    LastError   *errors.Error
    backoff     *Backoff
}

func NewRetryState(cfg RetryConfig) *RetryState {
    return &RetryState{
        backoff: NewBackoff(cfg.InitialDelay, cfg.MaxDelay, cfg.BackoffFactor, cfg.JitterFactor),
    }
}

// RecordSuccess registreert een succesvolle operatie
func (r *RetryState) RecordSuccess() {
    now := time.Now()
    r.LastSuccess = now
    r.LastError = nil
}

// RecordFailure registreert een fout en bepaalt of retry mogelijk is
func (r *RetryState) RecordFailure(err *errors.Error, cfg RetryConfig) (shouldRetry bool, delay time.Duration) {
    now := time.Now()

    // Reset attempts als vorige run stabiel was
    if !r.LastSuccess.IsZero() && now.Sub(r.LastSuccess) >= cfg.StableThreshold {
        r.Attempts = 0
        r.backoff.Reset()
    }

    r.Attempts++
    r.LastAttempt = now
    r.LastError = err

    // Check of retry zinvol is
    if !err.Retryable {
        return false, 0
    }

    if r.Attempts >= cfg.MaxAttempts {
        return false, 0
    }

    return true, r.backoff.Next()
}

// Exhausted geeft aan of max attempts bereikt is
func (r *RetryState) Exhausted(cfg RetryConfig) bool {
    return r.Attempts >= cfg.MaxAttempts
}
```

---

## 4. API Error Responses

### JSON Response Format

```go
// APIError is het response format voor errors
type APIError struct {
    Code    errors.ErrorCode `json:"code"`
    Message string           `json:"message"`
    Details []FieldError     `json:"details,omitempty"`
}

type FieldError struct {
    Field   string `json:"field"`
    Code    string `json:"code"`
    Message string `json:"message"`
}
```

### HTTP Status Mapping

| Error Code | HTTP Status |
|------------|-------------|
| `VALIDATION_ERROR` | 400 Bad Request |
| `NOT_FOUND` | 404 Not Found |
| `INVALID_STATE` | 409 Conflict |
| `CONNECTION_ERROR` | 502 Bad Gateway |
| `TIMEOUT` | 504 Gateway Timeout |
| `WEBHOOK_ERROR` | 502 Bad Gateway |
| `EMAIL_ERROR` | 502 Bad Gateway |
| `S3_ERROR` | 502 Bad Gateway |
| Default | 500 Internal Server Error |

### Response Voorbeelden

**Validation Error:**
```json
{
    "error": {
        "code": "VALIDATION_ERROR",
        "message": "validation failed",
        "details": [
            {"field": "host", "code": "REQUIRED", "message": "host is required"},
            {"field": "port", "code": "RANGE", "message": "port must be between 1 and 65535"}
        ]
    }
}
```

**Connection Error:**
```json
{
    "error": {
        "code": "CONNECTION_ERROR",
        "message": "SRT connection refused"
    }
}
```

---

## 5. Gebruik in Components

### Output Manager

```go
func (m *Manager) handleProcessExit(outputID string, exitErr error, stderr string) {
    // Parse FFmpeg error naar typed error
    appErr := errors.ParseFFmpegError(stderr, "output").
        With("output_id", outputID)

    // Record failure en bepaal retry
    shouldRetry, delay := m.retryStates[outputID].RecordFailure(appErr, m.retryConfig)

    // Log met structured context
    slog.Error("output failed",
        "output_id", outputID,
        "error_code", appErr.Code,
        "message", appErr.Message,
        "retryable", appErr.Retryable,
        "attempt", m.retryStates[outputID].Attempts,
    )

    // Broadcast status update
    m.broadcastStatus(outputID, types.ProcessStatus{
        State:      types.ProcessError,
        Error:      appErr.Message,
        ErrorCode:  string(appErr.Code),
        RetryCount: m.retryStates[outputID].Attempts,
        Exhausted:  !shouldRetry,
    })

    if shouldRetry {
        time.AfterFunc(delay, func() {
            m.startOutput(outputID)
        })
    }
}
```

### Encoder Source Loop

```go
func (e *Encoder) runSourceLoop() {
    retryState := resilience.NewRetryState(resilience.SourceRetryConfig)

    for {
        select {
        case <-e.ctx.Done():
            return
        default:
        }

        err := e.runCapture()
        if err == nil {
            retryState.RecordSuccess()
            continue
        }

        appErr := errors.ParseFFmpegError(err.Error(), "encoder")
        shouldRetry, delay := retryState.RecordFailure(appErr, resilience.SourceRetryConfig)

        slog.Error("source capture error",
            "error_code", appErr.Code,
            "message", appErr.Message,
            "attempt", retryState.Attempts,
        )

        if !shouldRetry {
            e.setLastError(fmt.Sprintf("Stopped after %d failed attempts: %s",
                retryState.Attempts, appErr.Message))
            return
        }

        time.Sleep(delay)
    }
}
```

---

## Samenvatting

| Aspect | Beschrijving |
|--------|--------------|
| **Error Type** | Typed `Error` met Code, Message, Cause, Retryable, Severity, Context |
| **FFmpeg Parsing** | Pattern matching voor bekende errors, fallback naar laatste regel |
| **Retry Logic** | Unified `RetryState` met configurable policy |
| **API Responses** | Structured JSON met code en details |
| **Logging** | Structured slog met error_code en context |

### Voordelen

1. **Programmatische handling** - Error codes maken conditonele logica mogelijk
2. **Betere observability** - Structured logging met consistente velden
3. **Unified retry** - Geen duplicatie, consistent gedrag
4. **Frontend-friendly** - API responses met codes voor UI feedback
5. **Extensible** - Nieuwe patterns/codes eenvoudig toe te voegen
