# ZuidWest FM Encoder

Audio streaming software for [ZuidWest FM](https://www.zuidwestfm.nl/) (Linux), [Radio Rucphen](https://www.rucphenrtv.nl/) (Linux) and [BredaNu](https://www.bredanu.nl/) (Windows, also the sole user of the Zabbix integration). Stream audio from a Raspberry Pi to remote SRT destinations, or expose local SRT pull listeners for multiple clients. Built for broadcast environments with real-time monitoring and web-based configuration.

<img src="https://github.com/oszuidwest/rpi-audio-encoder/assets/6742496/9070cb82-23be-4a31-8342-6607545d50eb" alt="Raspberry Pi and SRT logo" width="50%">

## Key features

- **Multi-output streaming** - Push to SRT servers or expose local endpoints for monitoring
- **Multi-recorder support** - Run multiple recording jobs with local and/or S3 storage, hourly rotation, retention cleanup and on-demand control
- **Configurable VU meters** - Peak/RMS metering with configurable peak hold and clip detection
- **Silence detection** - Alerts via webhook, email, file log, or Zabbix when audio drops below threshold
- **Audio incident dumps** - Capture audio around silence and channel imbalance events for troubleshooting
- **Channel imbalance detection** - Detects dead or mismatched L/R channels with webhook, email, file log, Zabbix, live UI, and readiness status
- **Multiple codecs** - MP3, Opus, or uncompressed PCM per output
- **Single binary** - Web interface embedded, minimal runtime dependencies

## Platform Support

| Platform | Status | Audio Capture |
|----------|--------|---------------|
| Linux (Raspberry Pi) | **Primary** | arecord (ALSA) |
| macOS | Development only | FFmpeg (AVFoundation) |
| Windows | Experimental | FFmpeg (DirectShow) |

The Windows build launches as a systray application without a console window: right-click the tray icon for Open UI / Start / Stop / Show Logs / Quit. slog output is redirected to `%PROGRAMDATA%\encoder\encoder.log` when no console is attached, falling back to `%LOCALAPPDATA%\encoder\encoder.log` and then the system temp directory if that path is not writable; the operational event log stays at `%PROGRAMDATA%\encoder\logs\<port>\encoder.jsonl`.

## Deployment

This is bare-metal software for a Raspberry Pi with a HiFiBerry sound card. Audio capture goes directly through ALSA (`arecord`). Install via the curl script in [Installation](#installation) below. CI publishes the binary as a GitHub release asset; no container image is published.

## Requirements

- Raspberry Pi 4 or 5
- [HiFiBerry Digi+ I/O](https://www.hifiberry.com/shop/boards/hifiberry-digi-io/) or [HiFiBerry DAC+ ADC](https://www.hifiberry.com/shop/boards/dacplus-adc/)
- Raspberry Pi OS Trixie Lite (64-bit)
- `ffmpeg` (for encoding; SRT protocol support is only required for push/caller streams)
- `alsa-utils` (for audio capture via `arecord`)

## Installation

1. Install Raspberry Pi OS Trixie Lite (64-bit)
2. Configure HiFiBerry following the [official guide](https://www.hifiberry.com/docs/software/configuring-linux-3-18-x/)
3. Run the installer as root:

```bash
sudo su
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/zwfm-encoder/main/deploy/install.sh)"
```

The web interface is available at `http://<raspberry-pi-ip>:8080` on your trusted LAN or through your VPN. New installations bind to `0.0.0.0:8080` by default; no domain name, certificate or reverse proxy is required. Choose local-only access in the installer if you plan to use a proxy or SSH tunnel. See [Admin security](#admin-security) for network restrictions and optional HTTPS setup.

**Default credentials:** `admin` / `encoder`

New installations are reachable from the LAN right away, so replace the default password before connecting the device to a shared network.

## Updating

Run the same installer script to update to the latest version:

```bash
sudo su
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/zwfm-encoder/main/deploy/install.sh)"
```

The installer detects existing installations and asks whether to:
- **Update only** - Downloads the latest binary while preserving your configuration
- **Fresh install** - Overwrites configuration (existing config is backed up)

The web interface shows a notification when updates are available.

## Audio Input

Connect the audio output of your audio processor to the HiFiBerry input.

**Requirements:**
- 48 kHz sample rate
- 16-bit depth
- Stereo (2 channels)

### Dante-compatible input

The encoder can receive two Dante-compatible transmitter channels directly in the Go process. It discovers the named channels over mDNS, requests a unicast flow from the transmitter, reorders media packets, fills missing packets with silence, and converts 16/24/32-bit network PCM to the encoder's 48 kHz stereo S16LE format. No Dante Virtual Soundcard or other external software is needed.

Set `audio.input` in `config.json` to a Dante-compatible input URI:

```json
{
  "audio": {
    "input": "dante://studio-tx/Program%20L/Program%20R?interface=eth0"
  }
}
```

The URI format is `dante://TRANSMITTER/LEFT_CHANNEL/RIGHT_CHANNEL?interface=INTERFACE`. Percent-encode spaces and other reserved characters in channel names.

Device and channel names are limited to 31 ASCII characters. Device names contain only letters, digits and hyphens, and cannot start or end with a hyphen; do not include a port or `.local` suffix. Channel names use printable ASCII except `=`, `.` and `@`. Spaces and percent-encoded `/`, `?` and `#` remain supported. Invalid names are rejected before discovery starts.

The web interface shows a configured Dante-compatible input in the Input Device selector; choosing another device there replaces it. Dante-compatible inputs are set in `config.json` only.

The interface is optional; when omitted, mDNS runs on all interfaces and the source address the OS routes toward the transmitter is used. The transmitter must run at 48 kHz and advertise 16-, 24-, or 32-bit PCM. The encoder and transmitter must be on a network where mDNS multicast and the Dante-compatible UDP traffic are permitted.

This receive-only Dante-compatible implementation is experimental and unofficial. It is not Dante-certified. It selects channels by name in the encoder configuration and does not advertise the encoder as a routable receiver in Dante Controller.

The compatible protocol behavior is described in [`docs/dante-protocol.md`](docs/dante-protocol.md).

The receiver was reimplemented using a two-team clean-room process: Claude produced the functional specification, and Codex implemented it in an isolated environment without network access or access to prior implementations.

`Dante` is a trademark of Audinate Pty Ltd. This unofficial project is not affiliated with, authorized, sponsored, certified, approved, or endorsed by Audinate.

## Codecs

| Codec | Encoder | Bitrate | Notes |
|-------|---------|---------|-------|
| MP3 | libmp3lame | 320 kbit/s | - |
| Opus | libopus | 128 kbit/s (64-256 configurable) | MPEG-TS container, 10 ms frames |
| PCM | s302m | ~1.92 Mbit/s (16-bit) | MPEG-TS container, SMPTE 302M |

## Silence Detection

Monitors audio levels and sends alerts when silence is detected or recovered. Uses hysteresis to prevent alert flapping:

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| Threshold | -40 dB | -60 to -1 | Audio level below which silence is detected |
| Duration | 15 s | 0.5 to 300 | Seconds of silence before alerting |
| Recovery | 5 s | 0.5 to 60 | Seconds of audio before recovery |

## Audio Input Recovery

If stopping an input exceeds the shutdown timeout, the encoder remains `stopping` and rejects a new start until the old source and its audio reader have exited. Once shutdown completes it becomes `stopped` and can be started again.

If the audio input disappears (a capture error, an unplugged USB interface, a Dante transmitter that stops sending), the encoder keeps reopening it. After a failed attempt it waits 3 seconds, doubling up to 10 seconds, plus up to 50% random jitter, so never more than 15 seconds. It never gives up, so audio resumes on its own when the input returns. A capture that keeps running but delivers no audio is treated as failed and reopened too: when no audio arrives within 30 seconds of opening the input, or audio stops for 10 seconds. Streams and recorders keep running in the meantime. The dashboard shows the reconnect attempts and the error of the last failed attempt.

The first failure of an outage writes an `input_lost` event and alerts every configured audio channel (webhook, email, Zabbix). Once the input has delivered steady audio (no pause over 2 seconds) for 10 seconds again, an `input_restored` event with the outage duration follows. Like abandoned uploads, these alerts go to every channel configured at that moment, regardless of per-event toggles. An open outage survives stopping and starting the encoder: it raises no second `input_lost`, and the next confirmed input, also from a newly selected device, closes it with a duration that includes the stopped time. After a process restart, the encoder restores an open outage from the latest input event in the retained event logs (including the rotated log). The next confirmed input then closes the incident and Zabbix problem as usual; an already-restored input does not produce another recovery. `GET /health` reports `503` between reopen attempts; while a reopened capture runs it reports `200`, even before that capture delivers audio.

## Channel Imbalance Detection

Detects dead or mismatched stereo channels by comparing the left and right RMS levels. Imbalance is only evaluated while at least one channel is above the configured silence threshold, so normal silence is handled by silence detection instead of imbalance detection.

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| Threshold | 12 dB | 1 to 59 | L/R level difference above which imbalance is detected |
| Duration | 15 s | 0.5 to 300 | Seconds of imbalance before alerting |
| Recovery | 5 s | 0.5 to 60 | Seconds of balanced audio, or dropped-away audio, before recovery |

Channel imbalance events can trigger webhook, email, and Zabbix notifications, are written to the event log, and are exposed in the dashboard, `GET /health`, and `GET /ready`. A confirmed imbalance makes the `channel_imbalance` readiness component fail until the channels are balanced again or audio drops away. When audio incident dumps are enabled, recovery also produces an MP3 with 15 seconds of context before and after the imbalance; the existing `audio_dump` webhook/email subscription controls its delivery.

## Alerting

**Audio alerting options** (can use multiple simultaneously, each with per-event control):
- **Webhook** - POST request to a URL (redirects are rejected, so configure the final URL); independently enable `silence_start`, `silence_end`, `audio_dump`, `channel_imbalance_start`, and `channel_imbalance_end`.
- **Email** - Microsoft Graph API notification; independently enable `silence_start`, `silence_end`, `audio_dump`, `channel_imbalance_start`, and `channel_imbalance_end`.
- **File Log** - Appends JSON Lines for every audio event, including silence and channel imbalance events (always records all events, no per-event toggle).
- **Zabbix** - Send trapper items to a Zabbix server; independently enable `silence_start`, `silence_end`, `channel_imbalance_start`, and `channel_imbalance_end` (no `audio_dump` - trapper items do not support file attachments).

`silence_end` and `channel_imbalance_end` are sent immediately when recovery is confirmed; `audio_dump_ready` is dispatched as a separate event once the MP3 context for either incident type finishes encoding. Its `trigger` field is `silence` or `channel_imbalance`. Lost and restored audio inputs and abandoned S3 uploads always alert every configured channel, regardless of per-event toggles.

Configure silence and channel imbalance detection under Settings -> Audio. Configure audio notifications under Settings -> Notifications.

### Microsoft 365 Email Setup

Email notifications use Microsoft Graph API with app-only authentication.

1. [Create an App Registration](https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade), then copy the **Client ID** and **Tenant ID**
2. Add API permissions: `Mail.Send` (required), `Application.Read.All` (optional, for secret expiry warnings)
3. Grant admin consent
4. Create a client secret, then copy the value immediately (won't be shown again)
5. [Create a shared mailbox](https://admin.exchange.microsoft.com/#/sharedmailboxes) as the sender (no license required)

The encoder warns when the secret expires within 30 days.

### Zabbix Setup

1. Import [`zabbix/template.xml`](https://github.com/oszuidwest/zwfm-encoder/blob/main/zabbix/template.xml) in Zabbix (**Data collection** -> **Templates** -> **Import**)
2. Link the template to your encoder host
3. Configure in the encoder: server, port (default 10051), host name (must match Zabbix exactly), silence key, imbalance key, and upload key

The template creates triggers for SILENCE (Disaster), RECOVERY (Info), INPUT_LOST (Disaster, recovers on INPUT_RESTORED), CHANNEL_IMBALANCE (High), CHANNEL_BALANCED (Info), TEST (Info), and UPLOAD_ABANDONED (High) events. Input events use the silence key; re-import the template to add the INPUT_LOST trigger. Channel imbalance uses a separate `imbalance.alert` item; Zabbix imbalance notifications only work when `imbalance_key` is configured and the matching trapper item exists.

## Hourly Recording

Recorder names must produce unique filename prefixes, compared case-insensitively. For example, `Studio` and `Studio!`, or `Studio B` and `Studio-B`, cannot be used together. Conflicting names are rejected when saving a recorder or loading an existing config; rename one of the conflicting recorders before starting the encoder.

Hourly recorders start a new file on every whole hour (whole UTC hours, which are the local hours in time zones with a whole-hour offset), named after the local time of the hand-over rounded to the minute, such as `<recorder>-YYYY-MM-DD-HH-00.<ext>`; the first file after a start is named after its start minute. At the boundary the next file is started before the current one is finalized, so the hand-over itself drops no audio; the previous file is then closed and queued for upload when S3 is configured. When the local clock repeats an hour, as at the end of daylight saving time, the second file of that hour gets a `-2` suffix instead of overwriting the first.

A successfully finalized run that received no PCM (for example during an input outage) is removed without uploading. Partial hours and digital silence are kept. Failed finalizations retain the file for inspection and report a recorder error; in S3-only mode such a retained file is still uploaded when the recorder is recreated at the next start.

On Linux, a recorder starts its first file only once the kernel reports the system clock as NTP-synchronized, because file names and retention cleanup come from the clock and a Raspberry Pi has no real-time clock. Until then it stays `starting` with the error `waiting for system clock synchronization`; a waiting hourly recorder makes `GET /ready` not ready, an on-demand recording can still be stopped, and streaming is not affected. The installer enables NTP. systemd-timesyncd and chrony (Debian's default `rtcsync`) mark the clock synchronized; a system that keeps time without such a client never starts recording, and ntpsec before Debian trixie marks the clock synchronized at startup, so it gives no protection. If the synchronization state cannot be read, recording starts with a warning.

## Recording API

Recorder startup, hourly file preparation and concurrent configuration changes do not hold up audio capture for streams or other recorders. During hourly preparation the current file keeps receiving audio until the replacement is ready. If an individual recorder's writer stops consuming audio, its bounded buffer drops new chunks and reports `audio_drops`; recording control operations may still wait for that recorder's filesystem.

S3 upload retries pause after 24 hours and raise an `upload_abandoned` alert. The recording and its upload metadata are retained, including in S3-only mode (in local+S3 mode, local retention still applies to the local copy). Fix S3 configuration/connectivity and restart the encoder process (not just the audio input) to re-queue abandoned uploads with their original object keys and a fresh retry window. There is no automatic disk-space eviction of unuploaded spool recordings: monitor free space and respond to upload alerts. On Linux the default spool is `/var/lib/encoder/recordings`, with recordings under `recorders/<id>/` and metadata under `uploads/<id>/`.

S3 storage must support `If-None-Match: *`, preserve user metadata, and allow reading object metadata via `HeadObject` (AWS requires `s3:GetObject`; see [HeadObject permissions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html)). The S3 connection test checks metadata round-tripping. An upload identity and its original object key are saved before any upload; retries, multipart completion failures and process restarts reuse that identity. A matching committed object counts as success, while a different recording gets a numeric suffix based on the original key. If an existing object cannot be verified, the local recording stays queued instead of being duplicated or discarded. Legacy objects without upload identities are treated as foreign, never overwritten; commits made by older versions cannot be retroactively identified.

On-demand recordings can be started and stopped via the REST API without going through the web interface. Useful for automation or external systems that need to trigger recordings.

```bash
# Start recording (recorder_id matches the ID in the web interface)
curl -X POST "http://<host>:8080/api/recordings/start?recorder_id=1" \
  -H "X-API-Key: <your-api-key>"

# Stop recording
curl -X POST "http://<host>:8080/api/recordings/stop?recorder_id=1" \
  -H "X-API-Key: <your-api-key>"
```

Configure the API key and maximum recording duration under Settings -> Audio -> Recording API. The `max_duration_minutes` limit automatically stops a recording after the configured time (0 = no limit).

For local recording storage under the packaged systemd service, use `/var/lib/encoder/recordings`. The service owns this state directory even with `ProtectSystem=strict`. Custom archive paths need a systemd override that adds the path to `ReadWritePaths=`.

## Configuration

Configuration is stored in `/etc/encoder/config.json` on production systems. For development, use the `-config` flag to specify a custom path, or place `config.json` next to the binary.

```json
{
  "system": { "bind_address": "0.0.0.0", "port": 8080, "username": "admin", "password": "encoder" },
  "web": { "station_name": "ZuidWest FM" }
}
```

The installer creates a minimal config file. All other settings are configured through the web interface.

The example above uses the legacy plaintext field for illustration; new installs and automatically created configs store a salted hash instead. Use the migration below for existing configs.

## Admin security

The encoder is intended to run as an internal appliance, managed directly by IP on a trusted LAN or through a VPN. Keep its web port restricted to the management network with your firewall/VPN rules; do not forward it from the public internet. HTTPS through a reverse proxy is optional. The encoder itself serves HTTP. The `/health` and `/ready` endpoints require no session and follow the same network restrictions.

### LAN and VPN access

The default `system.bind_address` is `0.0.0.0`, allowing access at `http://<device-ip>:8080` on all interfaces. Like `::` and an empty value, it uses Go's wildcard listener, which also accepts IPv6 where the OS allows dual-stack sockets. You can bind to a specific LAN or VPN interface by setting its numeric IPv4 or IPv6 address instead. A bind address must not include a port. Restart the encoder after changing the bind address or port.

Direct HTTP access relies on that trusted network for transport protection: passwords and session cookies are not encrypted by HTTP. A VPN encrypts traffic only as far as its endpoint; if it terminates at a router, the router-to-encoder hop still uses HTTP. Use the optional HTTPS setup below when that network path is not trusted or your policy requires browser-to-proxy encryption.

Password hashing, login limits and CSRF protection apply to both HTTP and HTTPS. CSRF protection remains useful on a private network because a malicious website can try to make an authenticated administrator's browser send requests to the appliance.

### Passwords and existing installations

Generate a hash using the encoder binary. Read the password from stdin, not a command-line argument or shell history (Bash example):

```bash
read -r -s -p 'Admin password: ' encoder_password
printf '\n'
printf '%s' "$encoder_password" | /usr/local/bin/encoder -hash-password
unset encoder_password
```

In `config.json`, replace `system.password` with `system.password_hash` containing the entire printed value; **remove the plaintext field**. Restart the encoder and verify login before deleting any recovery copy. Both fields together, malformed hashes and unsupported work factors are rejected. The command accepts one non-empty password line, preserves spaces, and does not load or modify configuration. It uses Go's standard-library PBKDF2-HMAC-SHA256 with a fresh 16-byte salt and 600,000 iterations, following the [OWASP PBKDF2 work-factor guidance](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html).

Existing plaintext credentials and omitted `bind_address` remain supported so an update does not lock out a LAN appliance; loading an existing config does not rewrite it. Migrate credentials explicitly using the steps above. New configs and fresh installer runs store hashed credentials and allow direct LAN/VPN access by default; the installer also offers local-only binding (`127.0.0.1`). An omitted or empty bind retains the legacy all-interface listener. If the installer downloads an older binary without `-hash-password`, it warns and retains the legacy plaintext field until you upgrade and migrate; older binaries also ignore `bind_address`, so keep those behind a firewall. Config files and old backups may contain other secrets, so keep them owner-only (`0600`) and protect/delete recovery copies according to your retention policy. Older releases cannot read `password_hash`; restore a plaintext `system.password` before downgrading.

The installer prompts for a non-empty password without echo. Non-interactive `WEB_PASSWORD` remains supported, but its export attribute is removed before any child process is started, and hashing receives it only on stdin. Failed steps report an error and warn when the encoder may still be stopped; inspect the installer output and service logs before restarting it. Installation is not an automatic rollback of a failed binary/configuration replacement.

### Optional HTTPS reverse proxy

If you want HTTPS, one option is Caddy on the same host with the following Caddyfile after setting the encoder's `bind_address` to `127.0.0.1`. Keep the proxy restricted to your management network too. Replace the domain and configure DNS and certificate issuance for your environment; see [Caddy's automatic HTTPS deployment requirements](https://caddyserver.com/docs/automatic-https). This example assumes a domain and a working certificate issuance method; neither is needed for the normal LAN/VPN installation.

```caddyfile
encoder.example.com {
    header Strict-Transport-Security "max-age=31536000"
    reverse_proxy 127.0.0.1:8080
}
```

Caddy's [reverse proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) supports WebSockets and sets `X-Forwarded-Proto`. Preserve the original `Host`, overwrite forwarded protocol headers at your trusted proxy, and do not expose the backend listener to untrusted peers. Session cookies use `HttpOnly`, `SameSite=Strict` and `Secure` for TLS/forwarded HTTPS requests. For a remote proxy, use a private bind address and firewall the backend to that proxy only.

For this same-host Caddy example, add `"trusted_proxies": ["127.0.0.1/32"]` to the existing `system` object, keeping its other fields. The default empty list trusts no proxy, including loopback. Only explicitly configured proxy CIDRs may supply `X-Forwarded-For` for login limits; the chain is read right-to-left until the first untrusted hop. Malformed forwarding data is rejected without consuming a shared proxy/account budget. Use exact proxy IPs (`/32` for IPv4, `/128` for IPv6) where possible, never a client subnet or all-address `/0` range. Invalid CIDRs and `/0` are rejected. If Caddy is itself behind another proxy, configure its [trusted proxy handling](https://caddyserver.com/docs/caddyfile/options#trusted-proxies) too; do not pass arbitrary client-supplied forwarding headers through.

### Optional local-only access and SSH tunnel

For local-only access, choose no at the installer's LAN/VPN prompt or set `system.bind_address` to `127.0.0.1` and restart. You can then tunnel from your workstation with `ssh -L 8080:127.0.0.1:8080 user@raspberry-pi-ip` and open `http://127.0.0.1:8080` locally. For the Windows tray UI on the device, loopback works directly.

### Login limits and API clients

The server allows five login attempts per client IPv4 address or IPv6 `/64` and ten per submitted username per minute, including successful attempts. Only requests with a valid one-use login CSRF token consume this budget; both limits are reserved atomically before password verification. Throttled attempts receive HTTP 429 with `Retry-After: 60`; wait and reload the login form before retrying, because its submitted token has been consumed. Rejected attempts do not extend the fixed window. The bounded in-memory cache reclaims old unrelated entries that have not reached their limit when full, instead of refusing every new client, and resets on restart. Password-hash verification runs one login at a time, so it uses at most one CPU core; when too many logins wait, the server answers HTTP 503 with `Retry-After: 1`. Rejected logins are logged with the peer address and the reason; submitted usernames are not logged. Forwarded IPs are used only from `system.trusted_proxies`; without it, proxy clients share the peer-IP limit. A proxy can additionally apply its own per-client rate limit. Do not repeatedly restart the encoder to evade throttling.

The username budget is intentionally shared across clients to resist distributed guessing. An active attack can temporarily deny new logins for that account, without revoking existing sessions. Accepted limitation: there is no rate-limit bypass for loopback requests, because an SSH tunnel and a same-host reverse proxy can have the same immediate peer address. A tunnel alone therefore does not escape the account limit. For recovery, use SSH to restrict the attacking traffic at the management firewall/VPN/proxy, then wait up to one minute for the fixed window to expire and reload the login form; restarting audio is not required. This is not a substitute for the documented access-control boundary; do not expose password-only admin login directly to the public internet.

Malformed client addresses from a trusted proxy return HTTP 400 with a forwarding-configuration hint, not a misleading 429 or a retry delay. Neither the shared proxy nor account budget is consumed. Untrusted forwarding headers are still ignored.

WebSocket handshakes get the same cross-origin check as every other browser request, so a page on another port of the same host or on another private address cannot open the live socket. Preserve the original Host including its external port at a proxy. Non-browser clients may omit Origin but still need a valid session.

Login forms use one-use tokens that expire after ten minutes; the oldest form tokens may be evicted earlier when the bounded anonymous token pool is busy. Reload an expired form to obtain a fresh token. A full pool does not block new login forms.

Every session-authenticated mutation (POST/PUT/PATCH/DELETE) requires `X-CSRF-Token` matching that session's random token, and a mutation without a valid session gets `401` with a JSON error instead of a redirect to the login page; the authenticated page supplies it in `<meta name="csrf-token">`, and the UI sends it automatically. Tokens expire/revoke with the session, and responses containing them are not cached. Logout is `POST /logout` with this header; GET does not log out. Browser cross-origin unsafe requests are also rejected using Go's cross-origin protection, including login. This follows the [OWASP synchronizer-token pattern](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html).

Custom session-based clients must fetch the authenticated page and send its token with mutations. The separate recording automation endpoints (`/api/recordings/start` and `/api/recordings/stop`) still use `X-API-Key`, not session cookies, so they do not require a CSRF token. Existing sessions are in memory and end on restart.

## Streaming Modes

Streams support two SRT modes:

- **Push to server** (`mode: "caller"`): the encoder connects to a remote SRT listener using the configured host, port, codec, password, stream ID, and retry settings.
- **Local pull listener** (`mode: "listener"`): the encoder opens a local SRT port so clients can pull the stream, for example `srt://<encoder-ip>:9000?mode=caller`.

Listener streams bind to `0.0.0.0` by default, use a separate port per stream, and accept up to 16 clients. They do not use `stream_id`. Client disconnects do not restart the encoder. SRT encryption is optional. Empty passwords allow unencrypted connections; non-empty passwords must be 10-64 characters. Caller mode requires FFmpeg with `srt` protocol support, while listener mode uses GoSRT and does not.

## Event Log

The encoder logs all stream, audio, and recording events to a JSON Lines file for monitoring and debugging. Audio events include silence, audio dump, and channel imbalance events. Events are accessible via the web interface and REST API.

The active log rotates at 50 MiB and keeps one previous file. The web interface and REST API read recent events from both files without loading the full log into memory.

See [docs/events.md](docs/events.md) for the complete event reference.

## Health Endpoint

`GET /health` provides a public endpoint for monitoring tools (Kubernetes probes, load balancers, Prometheus, etc.).

**Healthy (200 OK)** requires both:
- Encoder state is `running` (audio capture active)
- FFmpeg binary is available on the system

**Unhealthy (503 Service Unavailable)** when either:
- Encoder state is `stopped`, `starting`, or `stopping`
- FFmpeg binary is not found

Note: Stream connection failures, silence detection, channel imbalance detection, and recorder errors do **not** affect health status. These are reported in the response body for informational purposes only.

Response example:

```json
{
  "status": "healthy",
  "encoder_state": "running",
  "stream_count": 2,
  "streams_stable": 2,
  "recorder_count": 1,
  "recorders_running": 1,
  "uptime_seconds": 9252,
  "silence_detected": false,
  "channel_imbalance_detected": false
}
```

No authentication required.

## Readiness Endpoint

`GET /ready` is a public production-readiness endpoint. It returns 200 only when the process is usable for broadcast monitoring: FFmpeg is available, the encoder is running, the current input has delivered PCM within the last 2 seconds with no open input outage, enabled caller streams are stable, no silence alarm is active, no channel imbalance alarm is active, enabled hourly recorders are running, no recorder is in error, and no recording uploads are pending retry. Local pull listener streams are excluded from production-readiness because a running listener means "waiting for clients", not "connected remote output".

The `input` component fails before the first PCM arrives, when audio stops arriving for over 2 seconds, and while an input outage awaits confirmation of stable audio after recovery. Opening the device again alone does not clear an outage. The encoder status exposes the same condition as `input_ready`. `/health` retains its process-health meaning and can still return 200 for an open device that is not delivering audio.

The readiness rollup is intentionally strict: one pending recording upload makes the overall endpoint return `503` because the archive path is degraded. A confirmed channel imbalance (for example one dead channel) makes the `channel_imbalance` component not ready, since the broadcast is degraded even though the encoder keeps running. Monitoring that should only page for live broadcast impact should alert on the relevant components in the JSON body, such as `process`, `input`, `streams`, `silence`, and `channel_imbalance`, instead of the aggregate `status`. Planned off-air periods or other intentional silence also make the `silence` component not ready while the silence alarm is active.

When any component is not ready, the endpoint returns 503 with component details:

```json
{
  "status": "not_ready",
  "components": {
    "uploads": {
      "ok": false,
      "message": "recording uploads are pending retry",
      "details": { "pending": 1, "max": 0 }
    }
  }
}
```

## Architecture

```mermaid
flowchart LR
    A[Audio Input]

    subgraph Capture["Capture"]
        B[arecord / FFmpeg]
    end

    subgraph Processing["Audio Processing"]
        D[Distributor]

        subgraph Metering["Metering"]
            M[RMS/Peak<br>Calculator]
            PH[Peak Hold<br>Configurable]
            CD[Clip<br>Detect]
        end

        subgraph Silence["Audio Incident Detection"]
            SD[Detector<br>Hysteresis]
            SDM[Shared Dump Manager<br>Ring Buffer]
        end

        subgraph Imbalance["Channel Imbalance"]
            ID[Detector<br>L/R Difference]
        end
    end

    subgraph Outputs["Streaming Outputs"]
        OM[Stream Manager<br>Retry + Backoff]
        F1[FFmpeg caller output]
        F2[FFmpeg listener encoder]
        FO[GoSRT fan-out]
        S1[Remote SRT listener]
        S2[Local SRT clients]
    end

    subgraph Recording["Recording"]
        RM[Recording Manager]
        R1[Hourly]
        R2[On-Demand]
        ST[(Storage)]
    end

    subgraph Alerts["Alerting"]
        SN[Alert Orchestrator]
        N1[Webhook]
        N2[Email]
        N4[Zabbix]
    end

    subgraph UI["Web UI"]
        WS[WebSocket<br>levels 10fps + status 3s]
    end

    subgraph EventLog["Event Log"]
        EL[JSON Lines<br>Logger]
    end

    %% Main audio flow
    A ==>|S/PDIF| B ==>|PCM| D

    %% Metering branch
    D ==>|PCM| M -->|dB| PH
    M -->|samples| CD
    PH -->|dB JSON| WS
    CD -->|clip JSON| WS

    %% Silence detection branch
    D -->|dB| SD
    D ==>|PCM| SDM
    SD -->|events| SDM
    SD -->|state JSON| WS

    %% Channel imbalance branch
    D -->|dB| ID
    ID -->|state JSON| WS
    ID -->|events| SDM

    %% Alerting
    SD -->|event| SN
    SDM -->|MP3| SN
    ID -->|event| SN
    SN -->|HTTP| N1
    SN -->|Graph API| N2
    SN -->|trapper| N4
    SN -->|audio events| EL

    %% Streaming
    D ==>|PCM| OM
    OM ==>|PCM| F1 -->|SRT caller| S1
    OM ==>|PCM| F2 -->|pipe| FO -->|SRT listener| S2
    OM -->|events| EL

    %% Recording
    D ==>|PCM| RM
    RM ==>|PCM| R1 -->|audio| ST
    RM ==>|PCM| R2 -->|audio| ST
    RM -->|events| EL
    RM -->|upload abandoned| SN
```

### Audio Flow

1. **Capture**: `arecord` (Linux) or FFmpeg (macOS/Windows) captures 48kHz 16-bit stereo PCM
2. **Distributor**: Processes PCM in ~100ms chunks, fans out to all consumers
3. **Metering**: Calculates RMS/peak levels in Go (no FFmpeg filters), holds peaks for a configurable duration (default 3000 ms), detects clipping at +/-32760
4. **Silence Detection**: Hysteresis-based detection with configurable threshold/duration/recovery. The shared dump manager buffers 15s audio context before/after silence events.
5. **Channel Imbalance Detection**: Hysteresis-based L/R balance detection with configurable threshold/duration/recovery. Reuses the silence threshold as a presence floor, reports live balance/imbalance values, and uses the shared dump manager for recovery context.
6. **Alerting**: Silence and channel imbalance events trigger webhook, email (MS Graph), log (JSON Lines), and/or Zabbix. Webhook and email support per-event subscriptions for `silence_start`, `silence_end`, `audio_dump`, `channel_imbalance_start`, and `channel_imbalance_end`; Zabbix supports the same set except `audio_dump`. `silence_end` and `channel_imbalance_end` fire immediately when recovery is confirmed; `audio_dump_ready` fires separately once the MP3 is ready. Abandoned S3 uploads also trigger notifications.
7. **Streaming**: Per-output FFmpeg encoders; caller streams use FFmpeg SRT with retry/backoff, listener streams use a GoSRT multi-client fan-out around one FFmpeg stdout pipe
8. **Recording**: Hourly rotation or on-demand, with optional S3 upload
9. **Event Log**: All stream, audio, and recording events written to a JSON Lines file; accessible via web UI and REST API with pagination

## Post-installation

Optional hardening:

```bash
echo "dtoverlay=disable-wifi" >> /boot/firmware/config.txt  # Disable WiFi
apt remove bolt bluez ntfs-3g telnet                        # Remove unused packages
```

## SRT Resources

- [SRT Overview (IETF)](https://datatracker.ietf.org/meeting/107/materials/slides-107-dispatch-srt-overview-01)
- [SRT Deployment Guide](https://www.vmix.com/download/srt_alliance_deployment_guide.pdf)
- [SRT 101 Video](https://www.youtube.com/watch?v=e5YLItNG3lA)

## Related

- [Liquidsoap Server](https://github.com/oszuidwest/liquidsoap-ubuntu) - Companion server software for receiving SRT streams

## License

MIT License - See [LICENSE.md](LICENSE.md)
