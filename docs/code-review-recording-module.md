# Code Review: Recording Module

**Datum:** 2025-12-29
**Reviewer:** Claude Code (Go Architect Analysis)
**Scope:** `internal/recording/`, gerelateerde types en commands
**Methodologie:** Effective Go principes, KISS, DRY, YAGNI

---

## Executive Summary

De recording module bevat **ernstige architecturale problemen** die immediate aandacht vereisen. De belangrijkste issues zijn:

- **Goroutine leaks** bij foutcondities
- **Dead code** die disk cleanup verhindert
- **Dubbele cleanup systemen** met race conditions
- **Lock contention** die audio dropouts kan veroorzaken

Geschatte impact: ~170 regels code kunnen worden verwijderd/vereenvoudigd.

---

## Kritieke Problemen (P0 - Immediate Fix)

### 1. Dead Code: `uploadedFiles` Map Nooit Gevuld

**Locatie:** `recorder.go:68, 92, 577-586`

**Probleem:** De `uploadedFiles` map wordt geinitialiseerd maar **nooit gevuld**. De `cleanupOldFiles()` functie leest dus altijd van een lege map.

```go
// recorder.go:68 - GEDEFINIEERD
uploadedFiles map[string]time.Time // path -> upload time

// recorder.go:92 - GEINITIALISEERD
uploadedFiles: make(map[string]time.Time),

// recorder.go:577-586 - LEEST VAN LEGE MAP
func (r *GenericRecorder) cleanupOldFiles() {
    cutoff := time.Now().Add(-1 * time.Hour)
    r.cleanupMu.Lock()
    defer r.cleanupMu.Unlock()

    for path, uploadTime := range r.uploadedFiles { // ALTIJD LEEG!
        if uploadTime.Before(cutoff) {
            os.Remove(path)
            delete(r.uploadedFiles, path)
        }
    }
}
```

**Impact:**
- Disk loopt vol omdat retention cleanup niet werkt
- 10+ regels dode code
- Extra mutex (`cleanupMu`) voor niet-gebruikte data
- `cleanupWorker` goroutine draait elke 10 minuten voor niets

**Oplossing:** Verwijder `uploadedFiles`, `cleanupMu`, `cleanupWorker()` en `cleanupOldFiles()`. De Manager's `cleanupScheduler` (cleanup.go:22-77) handelt retention al correct af.

---

### 2. Goroutine Leak bij Start() Failure

**Locatie:** `recorder.go:161-166`

**Probleem:** Goroutines worden gestart VOORDAT de encoder succesvol is gestart. Als `startEncoderLocked()` faalt, blijven de goroutines forever wachten.

```go
func (r *GenericRecorder) Start() error {
    r.mu.Lock()
    defer r.mu.Unlock()

    // ... directory creation ...

    // GOROUTINES GESTART
    r.uploadWg.Add(1)
    go r.uploadWorker()    // Wacht op <-r.uploadStopCh

    r.uploadWg.Add(1)
    go r.cleanupWorker()   // Wacht op <-r.uploadStopCh

    // ENCODER KAN NOG FALEN!
    if err := r.startEncoderLocked(); err != nil {
        return err  // LEAK: uploadStopCh wordt nooit gesloten
    }

    // ...
}
```

**Impact:**
- Memory leak door orphaned goroutines
- Goroutines wachten forever op channel dat nooit gesloten wordt
- Bij herhaalde Start() attempts stapelen goroutines op

**Oplossing:** Start goroutines ALLEEN na succesvolle encoder start:

```go
func (r *GenericRecorder) Start() error {
    r.mu.Lock()
    defer r.mu.Unlock()

    // ... directory creation ...

    // EERST encoder starten
    if err := r.startEncoderLocked(); err != nil {
        return err  // Geen leak
    }

    // PAS DAARNA goroutines
    r.uploadWg.Add(1)
    go r.uploadWorker()

    // ...
}
```

---

### 3. Dubbel Cleanup Systeem

**Locaties:**
- `recorder.go:553-589` (GenericRecorder.cleanupWorker)
- `cleanup.go:22-77` (Manager.cleanupScheduler)

**Probleem:** Twee onafhankelijke cleanup systemen voor dezelfde data:

| Systeem | Interval | Threshold | Scope |
|---------|----------|-----------|-------|
| GenericRecorder.cleanupWorker | 10 minuten | 1 uur oud | Per recorder |
| Manager.cleanupScheduler | 24 uur (03:00) | RetentionDays | Alle recorders |

```go
// recorder.go:553-568 - PER-RECORDER CLEANUP
func (r *GenericRecorder) cleanupWorker() {
    ticker := time.NewTicker(10 * time.Minute)
    for {
        select {
        case <-r.uploadStopCh:
            return
        case <-ticker.C:
            r.cleanupOldFiles()  // 1 uur threshold
        }
    }
}

// cleanup.go:22-44 - MANAGER CLEANUP
func (m *Manager) startCleanupScheduler() {
    go func() {
        for {
            // Runs at 03:00 daily
            m.runCleanup()  // RetentionDays threshold
        }
    }()
}
```

**Impact:**
- Race condition: beide kunnen tegelijk dezelfde files verwijderen
- Onduidelijke verantwoordelijkheid
- Overbodige goroutines en timers

**Oplossing:** Verwijder recorder-level cleanup volledig. Manager cleanup is de juiste plaats voor retention-based deletion.

---

### 4. Lock Held During Slow I/O Operations

**Locatie:** `recorder.go:135-180`

**Probleem:** De mutex wordt vastgehouden tijdens langzame operaties:

```go
func (r *GenericRecorder) Start() error {
    r.mu.Lock()
    defer r.mu.Unlock()  // LOCK VOOR HELE FUNCTIE (~seconds)

    // SLOW: File system I/O
    if err := os.MkdirAll(outputDir, 0o755); err != nil {
        return err
    }

    // SLOW: FFmpeg process creation, network init
    if err := r.startEncoderLocked(); err != nil {
        return err
    }

    // ...
}
```

**Impact:**
- `WriteAudio()` (line 222) kan niet RLock nemen tijdens Start()
- Audio data gaat verloren (dropout)
- Systeem wordt unresponsive

**Oplossing:** Split Start() in fasen met minimale lock scope:

```go
func (r *GenericRecorder) Start() error {
    // Phase 1: Validate state (short lock)
    r.mu.Lock()
    if r.state == StateRecording {
        r.mu.Unlock()
        return nil
    }
    r.state = StateStarting
    r.mu.Unlock()

    // Phase 2: Slow operations (no lock)
    if err := os.MkdirAll(outputDir, 0o755); err != nil {
        return err
    }

    // Phase 3: Start encoder and update state (short lock)
    r.mu.Lock()
    defer r.mu.Unlock()
    return r.startEncoderLocked()
}
```

---

## Hoge Prioriteit (P1)

### 5. `error` Field Shadows Built-in Type

**Locatie:** `recorder.go:33`

```go
type GenericRecorder struct {
    state   RecordingState
    error   string  // SHADOWED NAME!
}
```

**Probleem:** Het veld `error` shadowed de built-in `error` interface. Dit maakt code verwarrend en kan type conversie problemen veroorzaken.

**Oplossing:** Rename naar `lastError` (consistent met `lastUploadErr`).

---

### 6. ErrAlreadyRecording Defined but Not Used

**Locaties:** `types.go:12`, `recorder.go:138-140`

```go
// types.go:12 - DEFINED
var ErrAlreadyRecording = errors.New("recorder is already recording")

// recorder.go:138-140 - NOT USED!
if r.state == StateRecording {
    return nil  // Zou ErrAlreadyRecording moeten returnen
}
```

**Oplossing:** Gebruik de sentinel error:

```go
if r.state == StateRecording {
    return ErrAlreadyRecording
}
```

---

### 7. S3Config Struct Duplicatie (DRY Violation)

**Locaties:** `recorder.go:693-699, 704-709, 732-737`, `commands.go:716-721`

Dezelfde S3Config wordt op 4 plaatsen handmatig opgebouwd:

```go
// Pattern herhaald 4x
cfg := &S3Config{
    Endpoint:        r.config.S3Endpoint,
    Bucket:          r.config.S3Bucket,
    AccessKeyID:     r.config.S3AccessKeyID,
    SecretAccessKey: r.config.S3SecretAccessKey,
}
```

**Oplossing:** Add helper method aan types.Recorder:

```go
// types.go
func (r *Recorder) ToS3Config() *recording.S3Config {
    return &recording.S3Config{
        Endpoint:        r.S3Endpoint,
        Bucket:          r.S3Bucket,
        AccessKeyID:     r.S3AccessKeyID,
        SecretAccessKey: r.S3SecretAccessKey,
    }
}

// Usage
return createS3Client(r.config.ToS3Config())
```

---

### 8. Codec/Format Logic Duplicatie

**Locatie:** `recorder.go:654-683`

`recorder.go` definieert eigen `getFileExtension()` en `getContentType()` terwijl `types.go` al `CodecPresets` heeft met dezelfde informatie.

```go
// recorder.go - DUPLICAAT
func (r *GenericRecorder) getFileExtension() string {
    switch r.config.Codec {
    case "mp2": return "mp2"
    case "mp3": return "mp3"
    // ...
    }
}

// types.go - BRON VAN WAARHEID
var CodecPresets = map[string]CodecPreset{
    "mp2": {Args: [...], Format: "mp2"},
    "mp3": {Args: [...], Format: "mp3"},
    // ...
}
```

**Oplossing:** Add methods aan Recorder type (zoals Output al heeft):

```go
// types.go
func (r *Recorder) CodecArgs() []string {
    if preset, ok := CodecPresets[r.Codec]; ok {
        return preset.Args
    }
    return CodecPresets[DefaultCodec].Args
}

func (r *Recorder) Format() string {
    if preset, ok := CodecPresets[r.Codec]; ok {
        return preset.Format
    }
    return CodecPresets[DefaultCodec].Format
}
```

---

### 9. Context Cancellation Incomplete

**Locatie:** `recorder.go:394-416`

```go
select {
case err := <-done:
    // Process finished
case <-time.After(10 * time.Second):
    r.cancel()  // Context canceled, maar geen verify!
}
```

**Probleem:** Na context cancellation is er geen garantie dat FFmpeg daadwerkelijk stopt. Geen tweede timeout met force kill.

**Oplossing:**

```go
select {
case err := <-done:
    // Process finished gracefully
case <-time.After(10 * time.Second):
    r.cancel()
    // Wait for graceful shutdown
    select {
    case <-done:
        // Stopped after cancel
    case <-time.After(2 * time.Second):
        r.cmd.Process.Kill()  // Force kill
        slog.Error("force killed ffmpeg", "id", r.id)
    }
}
```

---

### 10. uploadQueue Never Closed

**Locatie:** `recorder.go:90, 209, 468-477`

```go
// Created
uploadQueue: make(chan uploadRequest, 100)

// In Stop() - ONLY uploadStopCh closed
close(r.uploadStopCh)
r.uploadWg.Wait()

// uploadQueue NEVER closed!
```

**Probleem:** uploadWorker relied op default case om te stoppen, niet op channel closure. Dit is fragile.

**Oplossing:** Close uploadQueue in Stop():

```go
func (r *GenericRecorder) Stop() error {
    // ...
    close(r.uploadQueue)   // Signal no more items
    close(r.uploadStopCh)  // Signal workers to stop
    r.uploadWg.Wait()
}
```

---

## YAGNI Violations (P2)

### 11. RotationWindow Staggering (Premature Optimization)

**Locatie:** `manager.go:14, 85-106`

```go
const RotationWindow = 30 * time.Second

func (m *Manager) calculateRotationOffsetLocked(cfg *types.Recorder) time.Duration {
    // Complex math to spread hourly rotations across 30 seconds
    hourlyCount := 0
    for _, rec := range m.recorders { /* count */ }
    totalCount := hourlyCount + 1
    offset := time.Duration(float64(hourlyCount) / float64(totalCount) * float64(RotationWindow))
    return offset
}
```

**Vraag:** Hoeveel hourly recorders draai je realistisch? Met 3-5 recorders is de "I/O spike" verwaarloosbaar. Dit is premature optimization.

**Aanbeveling:** Verwijder de offset logic. Alle hourly recorders roteren op hour boundary - dat is expected behavior.

---

### 12. EncoderController Interface (Single Implementation)

**Locatie:** `server/commands.go:35-51`

```go
type EncoderController interface {
    State() types.EncoderState
    Start() error
    Stop() error
    Restart() error
    StartOutput(outputID string) error
    StopOutput(outputID string) error
    // ... 15 methods total
}
```

**Probleem:** Interface heeft slechts één implementatie (`*encoder.Encoder`). Per YAGNI: interfaces zijn alleen nodig bij meerdere implementaties.

**Aanbeveling:** Verwijder interface, gebruik `*encoder.Encoder` direct.

---

### 13. statusCallback is Empty

**Locaties:** `main.go:56-59`, `recorder.go:64, 423, 548, 624`

```go
// main.go - EMPTY CALLBACK
enc.SetStatusCallback(func() {
    // Status updates are handled via WebSocket polling
})

// recorder.go - CALLED 6x
if r.statusCallback != nil {
    r.statusCallback()  // DOET NIETS
}
```

**Aanbeveling:** Verwijder volledig - statusCallback, SetStatusCallback(), en alle nil checks.

---

### 14. lastUploadTime/lastUploadErr Tracked but Inaccessible

**Locatie:** `recorder.go:54-55, 521-531`

Velden worden bijgewerkt maar nooit exposed via WebSocket/API:

```go
// Updated in uploadFile()
r.lastUploadTime = &now
r.lastUploadErr = ""

// NOT in RecorderStatus struct!
type RecorderStatus struct {
    State    string
    Duration float64
    Error    string
    // lastUploadTime NOT HERE
}
```

**Aanbeveling:** Ofwel toevoegen aan RecorderStatus, ofwel verwijderen.

---

## Go Idioms Schendingen (P3)

### 15. Pointer Receivers on Immutable Methods

**Locatie:** `types.go:54-71`

```go
// Current - pointer receiver voor read-only method
func (o *Output) IsEnabled() bool {
    return o.Enabled == nil || *o.Enabled
}

// Idiomatic Go - value receiver
func (o Output) IsEnabled() bool {
    return o.Enabled == nil || *o.Enabled
}
```

**Toepassen op:** `Output.IsEnabled()`, `Output.MaxRetriesOrDefault()`, `Recorder.IsEnabled()`, `Recorder.CodecArgs()`, `Recorder.Format()`

---

### 16. Nullable Boolean Anti-Pattern

**Locatie:** `types.go:43, 148`

```go
type Output struct {
    Enabled *bool `json:"enabled,omitempty"`  // nil = true (confusing!)
}

func (o *Output) IsEnabled() bool {
    return o.Enabled == nil || *o.Enabled  // Three states: nil, true, false
}
```

**Probleem:** `*bool` is verwarrend - nil betekent true, wat counter-intuitive is.

**Alternatieven:**
1. Gebruik `bool` met explicit default in constructor
2. Gebruik typed enum voor tristate

---

### 17. Constants Scattered

**Locatie:** `types.go:9-142`

Constants zijn willekeurig verspreid door het bestand.

**Aanbeveling:** Groepeer per domein:

```go
// Encoder state machine
const (
    StateStopped  EncoderState = "stopped"
    StateStarting EncoderState = "starting"
    // ...
)

// Retry configuration
const (
    DefaultMaxRetries = 99
    InitialRetryDelay = 3 * time.Second
    MaxRetryDelay     = 60 * time.Second
)

// Codec defaults
const (
    DefaultCodec         = "wav"
    DefaultRetentionDays = 90
)
```

---

## Aanbevolen Actieplan

### Phase 1: Critical Bug Fixes (Immediate)

| # | Item | Effort | Files |
|---|------|--------|-------|
| 1 | Fix goroutine leak in Start() | 15 min | recorder.go |
| 2 | Remove uploadedFiles + cleanupWorker | 20 min | recorder.go |
| 3 | Rename error field to lastError | 10 min | recorder.go |
| 4 | Use ErrAlreadyRecording sentinel | 5 min | recorder.go |

### Phase 2: DRY Cleanup (This Week)

| # | Item | Effort | Files |
|---|------|--------|-------|
| 5 | Add Recorder.ToS3Config() helper | 15 min | types.go, recorder.go |
| 6 | Add Recorder.CodecArgs()/Format() | 20 min | types.go, recorder.go |
| 7 | Fix context cancellation (2-stage) | 15 min | recorder.go |
| 8 | Close uploadQueue properly | 10 min | recorder.go |

### Phase 3: Simplification (Later)

| # | Item | Effort | Files |
|---|------|--------|-------|
| 9 | Remove RotationWindow offset | 20 min | manager.go, recorder.go |
| 10 | Remove EncoderController interface | 15 min | commands.go |
| 11 | Remove statusCallback | 15 min | recorder.go, manager.go, main.go |
| 12 | Fix lock scope in Start() | 30 min | recorder.go |

---

## Metrics

| Categorie | Geschatte Lines | Items |
|-----------|-----------------|-------|
| Dead code te verwijderen | ~50 | uploadedFiles, cleanupWorker, statusCallback |
| Duplicatie te consolideren | ~80 | S3Config, codec logic, command handlers |
| Over-engineering te verwijderen | ~40 | RotationWindow, EncoderController |
| **TOTAAL** | **~170 lines** | Netto reductie |

---

## Effective Go Compliance Score

| Criterium | Score | Notities |
|-----------|-------|----------|
| Type Naming | 95% | `error` field shadows built-in |
| Interface Design | 60% | Geen interfaces gedefinieerd voor testability |
| Error Handling | 75% | Sentinel errors gedefinieerd maar niet consistent gebruikt |
| Concurrency | 50% | Goroutine leaks, lock contention, dual cleanup |
| Constants | 75% | Scattered organization |
| Zero Values | 60% | Nullable booleans, pointer-to-time |

**Overall: 70/100** - Functioneel maar met significante technische schuld.
