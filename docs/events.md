# Event Log Reference

This document describes all events emitted by the encoder to the event log (`encoder.jsonl`).

## Log File Location

| Platform | Path |
|----------|------|
| Linux/macOS | `/var/log/encoder/{port}/encoder.jsonl` |
| Windows | `%PROGRAMDATA%\encoder\logs\{port}\encoder.jsonl` |

## Common Event Structure

All events share this base structure:

```json
{
  "ts": "2024-01-15T14:30:00.123Z",
  "type": "event_type",
  "stream_id": "stream-abc123",
  "msg": "Optional message",
  "details": { }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `ts` | string | ISO 8601 timestamp |
| `type` | string | Event type identifier |
| `stream_id` | string | Stream identifier (stream events only) |
| `msg` | string | Optional human-readable message |
| `details` | object | Type-specific details (see below) |

## Severity Levels

Events are categorized by severity for UI display:

| Severity | Color | Description |
|----------|-------|-------------|
| `error` | Red | Critical failures requiring attention |
| `warning` | Orange | Temporary issues or degraded state |
| `success` | Green | Positive state changes or completions |
| `info` | Gray | Neutral informational events |

---

## Stream Events

Stream events track the lifecycle and health of audio output streams.

### Details Structure

```json
{
  "stream_name": "Main Stream",
  "error": "Connection refused",
  "retry": 3,
  "max_retries": 99
}
```

| Field | Type | Description |
|-------|------|-------------|
| `stream_name` | string | Human-readable stream name |
| `error` | string | Error message (if applicable) |
| `retry` | int | Current retry attempt number |
| `max_retries` | int | Maximum retry attempts configured |

---

### `stream_started`

- **Severity:** `info`
- **UI Label:** Started
- **Triggered:** When a stream begins connecting to its destination.

```json
{
  "ts": "2024-01-15T14:30:00.000Z",
  "type": "stream_started",
  "stream_id": "stream-4ce838a5",
  "details": {
    "stream_name": "Main Stream"
  }
}
```

---

### `stream_stable`

- **Severity:** `success`
- **UI Label:** Connected
- **Triggered:** When a stream has been running successfully for the stability threshold (default: 30 seconds).

```json
{
  "ts": "2024-01-15T14:30:30.000Z",
  "type": "stream_stable",
  "stream_id": "stream-4ce838a5",
  "details": {
    "stream_name": "Main Stream"
  }
}
```

---

### `stream_error`

- **Severity:** `error`
- **UI Label:** Error
- **Triggered:** When FFmpeg reports an error or the stream process exits unexpectedly.

```json
{
  "ts": "2024-01-15T14:31:00.000Z",
  "type": "stream_error",
  "stream_id": "stream-4ce838a5",
  "details": {
    "stream_name": "Main Stream",
    "error": "Error opening output files: Connection refused"
  }
}
```

---

### `stream_retry`

- **Severity:** `warning`
- **UI Label:** Retry
- **Triggered:** When a failed stream is about to retry connecting (after backoff delay).

```json
{
  "ts": "2024-01-15T14:31:06.000Z",
  "type": "stream_retry",
  "stream_id": "stream-4ce838a5",
  "details": {
    "stream_name": "Main Stream",
    "error": "Error opening output files: Connection refused",
    "retry": 1,
    "max_retries": 99
  }
}
```

---

### `stream_stopped`

- **Severity:** `info`
- **UI Label:** Stopped
- **Triggered:** When a stream is intentionally stopped (user action or encoder shutdown).

```json
{
  "ts": "2024-01-15T15:00:00.000Z",
  "type": "stream_stopped",
  "stream_id": "stream-4ce838a5",
  "details": {
    "stream_name": "Main Stream"
  }
}
```

---

## Silence Events

Silence events track periods when audio levels drop below the configured threshold.

### Details Structure

```json
{
  "level_left_db": -48.5,
  "level_right_db": -52.3,
  "threshold_db": -40.0,
  "duration_ms": 65000,
  "dump_path": "/var/log/encoder/8080/dumps",
  "dump_filename": "silence-2024-01-15-143100.mp3",
  "dump_size_bytes": 245760,
  "dump_error": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `level_left_db` | float | Left channel RMS level in dB |
| `level_right_db` | float | Right channel RMS level in dB |
| `threshold_db` | float | Configured silence threshold in dB |
| `duration_ms` | int | Silence duration in milliseconds (end event only) |
| `dump_path` | string | Directory where dump file is saved |
| `dump_filename` | string | Name of the audio dump file |
| `dump_size_bytes` | int | Size of dump file in bytes |
| `dump_error` | string | Error if dump failed |

---

### `silence_start`

- **Severity:** `warning`
- **UI Label:** Silence
- **Triggered:** When audio levels drop below threshold for the configured duration (hysteresis).

```json
{
  "ts": "2024-01-15T14:31:00.000Z",
  "type": "silence_start",
  "details": {
    "level_left_db": -48.5,
    "level_right_db": -52.3,
    "threshold_db": -40.0
  }
}
```

---

### `silence_end`

- **Severity:** `success`
- **UI Label:** Recovered
- **Triggered:** When audio levels return above threshold after a silence period.

```json
{
  "ts": "2024-01-15T14:32:05.000Z",
  "type": "silence_end",
  "details": {
    "level_left_db": -12.3,
    "level_right_db": -14.1,
    "threshold_db": -40.0,
    "duration_ms": 65000,
    "dump_path": "/var/log/encoder/8080/dumps",
    "dump_filename": "silence-2024-01-15-143100.mp3",
    "dump_size_bytes": 245760
  }
}
```

---

## Recorder Events

Recorder events track the lifecycle of audio recording, file uploads, and cleanup operations.

### Details Structure

```json
{
  "recorder_name": "Archive",
  "filename": "Archive-2024-01-15-14-00.mp3",
  "codec": "mp3",
  "storage_mode": "both",
  "s3_key": "recordings/Archive/Archive-2024-01-15-14-00.mp3",
  "error": "",
  "retry": 0,
  "files_deleted": 5,
  "storage_type": "local"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `recorder_name` | string | Human-readable recorder name |
| `filename` | string | Recording filename |
| `codec` | string | Audio codec: `mp3`, `mp2`, `ogg`, `wav` |
| `storage_mode` | string | Storage mode: `local`, `s3`, `both` |
| `s3_key` | string | S3 object key (for upload events) |
| `error` | string | Error message (if applicable) |
| `retry` | int | Retry attempt number (upload events only) |
| `files_deleted` | int | Number of files deleted (cleanup only) |
| `storage_type` | string | Storage cleaned: `local` or `s3` |

---

### `recorder_started`

- **Severity:** `info`
- **UI Label:** Started
- **Triggered:** When a recorder begins recording audio.

```json
{
  "ts": "2024-01-15T14:00:00.000Z",
  "type": "recorder_started",
  "details": {
    "recorder_name": "Archive",
    "codec": "mp3",
    "storage_mode": "both"
  }
}
```

---

### `recorder_stopped`

- **Severity:** `info`
- **UI Label:** Stopped
- **Triggered:** When a recorder stops recording (user action, encoder shutdown, or max duration reached).

```json
{
  "ts": "2024-01-15T15:00:00.000Z",
  "type": "recorder_stopped",
  "details": {
    "recorder_name": "Archive",
    "codec": "mp3",
    "storage_mode": "both"
  }
}
```

---

### `recorder_error`

- **Severity:** `error`
- **UI Label:** Error
- **Triggered:** When a recorder encounters an error (path not writable, FFmpeg failure, etc.).

```json
{
  "ts": "2024-01-15T14:00:05.000Z",
  "type": "recorder_error",
  "details": {
    "recorder_name": "Archive",
    "codec": "mp3",
    "storage_mode": "local",
    "error": "local path is not writable"
  }
}
```

---

### `recorder_file`

- **Severity:** `info`
- **UI Label:** New File
- **Triggered:** When a new recording file is created (at start or hourly rotation).

```json
{
  "ts": "2024-01-15T14:00:00.000Z",
  "type": "recorder_file",
  "details": {
    "recorder_name": "Archive",
    "filename": "Archive-2024-01-15-14-00.mp3",
    "codec": "mp3",
    "storage_mode": "both"
  }
}
```

---

### `upload_queued`

- **Severity:** `info`
- **UI Label:** Upload Queued
- **Triggered:** When a completed recording file is queued for S3 upload.

```json
{
  "ts": "2024-01-15T15:00:00.000Z",
  "type": "upload_queued",
  "details": {
    "recorder_name": "Archive",
    "filename": "Archive-2024-01-15-14-00.mp3",
    "codec": "mp3",
    "storage_mode": "both",
    "s3_key": "recordings/Archive/Archive-2024-01-15-14-00.mp3"
  }
}
```

---

### `upload_completed`

- **Severity:** `success`
- **UI Label:** Uploaded
- **Triggered:** When a file has been successfully uploaded to S3.

```json
{
  "ts": "2024-01-15T15:00:30.000Z",
  "type": "upload_completed",
  "details": {
    "recorder_name": "Archive",
    "filename": "Archive-2024-01-15-14-00.mp3",
    "codec": "mp3",
    "storage_mode": "both",
    "s3_key": "recordings/Archive/Archive-2024-01-15-14-00.mp3"
  }
}
```

---

### `upload_failed`

- **Severity:** `error`
- **UI Label:** Upload Failed
- **Triggered:** When an S3 upload fails. The file is added to a retry queue for later attempts.

```json
{
  "ts": "2024-01-15T15:00:30.000Z",
  "type": "upload_failed",
  "details": {
    "recorder_name": "Archive",
    "filename": "Archive-2024-01-15-14-00.mp3",
    "codec": "mp3",
    "storage_mode": "s3",
    "s3_key": "recordings/Archive/Archive-2024-01-15-14-00.mp3",
    "error": "AccessDenied: Access Denied",
    "retry": 0
  }
}
```

---

### `upload_retry`

- **Severity:** `warning`
- **UI Label:** Retry
- **Triggered:** When a failed upload is being retried. Retries occur at hour boundaries (during file rotation). Files are retried for up to 24 hours.

```json
{
  "ts": "2024-01-15T16:00:00.000Z",
  "type": "upload_retry",
  "details": {
    "recorder_name": "Archive",
    "filename": "Archive-2024-01-15-14-00.mp3",
    "codec": "mp3",
    "storage_mode": "s3",
    "s3_key": "recordings/Archive/Archive-2024-01-15-14-00.mp3",
    "retry": 1
  }
}
```

---

### `upload_abandoned`

- **Severity:** `error`
- **UI Label:** Abandoned
- **Triggered:** When an upload is abandoned after exceeding the 24-hour retry limit. The local file remains on disk but will no longer be retried.

```json
{
  "ts": "2024-01-16T15:00:00.000Z",
  "type": "upload_abandoned",
  "details": {
    "recorder_name": "Archive",
    "filename": "Archive-2024-01-15-14-00.mp3",
    "codec": "mp3",
    "storage_mode": "s3",
    "s3_key": "recordings/Archive/Archive-2024-01-15-14-00.mp3",
    "error": "exceeded 24h retry limit",
    "retry": 24
  }
}
```

---

### `cleanup_completed`

- **Severity:** `success`
- **UI Label:** Cleanup
- **Triggered:** When retention cleanup deletes old files (runs hourly).

```json
{
  "ts": "2024-01-15T15:00:00.000Z",
  "type": "cleanup_completed",
  "details": {
    "recorder_name": "Archive",
    "files_deleted": 24,
    "storage_type": "local"
  }
}
```

---

## API Access

Events can be retrieved via the REST API:

```
GET /api/events?limit=50&offset=0&type=stream
```

| Parameter | Type | Description |
|-----------|------|-------------|
| `limit` | int | Maximum events to return (default: 50, max: 500) |
| `offset` | int | Number of events to skip for pagination |
| `type` | string | Filter: `stream`, `silence`, `recorder`, or omit for all |

### Response

```json
{
  "events": [ ... ],
  "has_more": true
}
```

---

## Event Summary Table

| Event Type | Category | Severity | UI Label | Trigger |
|------------|----------|----------|----------|---------|
| `stream_started` | Stream | info | Started | Stream begins connecting |
| `stream_stable` | Stream | success | Connected | Stream stable for 30s |
| `stream_error` | Stream | error | Error | Stream encounters error |
| `stream_retry` | Stream | warning | Retry | Stream retrying after failure |
| `stream_stopped` | Stream | info | Stopped | Stream intentionally stopped |
| `silence_start` | Silence | warning | Silence | Audio below threshold |
| `silence_end` | Silence | success | Recovered | Audio returns above threshold |
| `recorder_started` | Recorder | info | Started | Recorder begins recording |
| `recorder_stopped` | Recorder | info | Stopped | Recorder stops recording |
| `recorder_error` | Recorder | error | Error | Recorder encounters error |
| `recorder_file` | Recorder | info | New File | New recording file created |
| `upload_queued` | Recorder | info | Upload Queued | File queued for S3 upload |
| `upload_completed` | Recorder | success | Uploaded | S3 upload successful |
| `upload_failed` | Recorder | error | Upload Failed | S3 upload failed |
| `upload_retry` | Recorder | warning | Retry | Failed upload being retried |
| `upload_abandoned` | Recorder | error | Abandoned | Upload abandoned after 24h |
| `cleanup_completed` | Recorder | success | Cleanup | Retention cleanup completed |
