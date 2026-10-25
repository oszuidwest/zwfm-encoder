# ZuidWest FM Encoder

Audio streaming software for [ZuidWest FM](https://www.zuidwestfm.nl/) (Linux), [Radio Rucphen](https://www.rucphenrtv.nl/) (Linux) and [BredaNu](https://www.bredanu.nl/) (Windows). Stream audio from a Raspberry Pi to remote SRT destinations, or expose local SRT pull listeners for multiple clients. Built for broadcast environments with real-time monitoring and web-based configuration.

<img src="https://github.com/oszuidwest/rpi-audio-encoder/assets/6742496/9070cb82-23be-4a31-8342-6607545d50eb" alt="Raspberry Pi and SRT logo" width="50%">

## Key features

- **Multi-output streaming** - Push to SRT servers or expose local SRT listeners
- **Multi-recorder support** - Hourly or on-demand recording to local disk and/or S3, with retention cleanup
- **VU meters** - Peak/RMS metering with configurable peak hold and clip detection
- **Silence and channel imbalance detection** - Alerts via webhook, email or Zabbix, with MP3 incident dumps
- **Input recovery** - Reopens a lost audio input automatically and alerts on the outage
- **Multiple codecs** - MP3, Opus, or uncompressed PCM per output
- **Single binary** - Web interface embedded, minimal runtime dependencies

## Platform Support

| Platform | Status | Audio Capture |
|----------|--------|---------------|
| Linux (Raspberry Pi) | **Primary** | arecord (ALSA) or [Dante-compatible network input](#dante-compatible-input) |
| macOS | Development only | FFmpeg (AVFoundation) |
| Windows | Experimental | FFmpeg (DirectShow) |

On Windows the encoder runs as a tray application and writes its log to `%PROGRAMDATA%\encoder\encoder.log`; Show Logs Folder opens the event log. Its recording spool, the upload queue and S3-only recordings, is `%PROGRAMDATA%\encoder\recordings`.

## Requirements

- Raspberry Pi 4 or 5
- [HiFiBerry Digi+ I/O](https://www.hifiberry.com/shop/boards/hifiberry-digi-io/) or [HiFiBerry DAC+ ADC](https://www.hifiberry.com/shop/boards/dacplus-adc/) (not needed with a Dante-compatible input)
- Raspberry Pi OS Trixie Lite (64-bit)
- `ffmpeg` (for encoding; SRT protocol support is only required for push streams)
- `alsa-utils` (for audio capture via `arecord`)

## Installation

1. Install Raspberry Pi OS Trixie Lite (64-bit)
2. Configure HiFiBerry following the [official guide](https://www.hifiberry.com/docs/software/configuring-linux-3-18-x/)
3. Run the installer as root:

```bash
sudo su
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/oszuidwest/zwfm-encoder/main/deploy/install.sh)"
```

The installer asks for the station name, admin username (default `admin`) and password, web port (default 8080) and whether to allow access from the LAN. The web interface is then available at `http://<raspberry-pi-ip>:8080`. See [Admin security](#admin-security) before connecting the device to a shared network.

## Updating

Run the same installer again. It detects the existing installation and offers:
- **Update only** - Installs the latest binary and service unit, keeping your configuration
- **Fresh install** - Overwrites the configuration (the existing config is backed up)

The service unit is replaced on update, so put local changes in a drop-in (`systemctl edit encoder`). The web interface shows a notification when a new release is available. Read the [release notes](https://github.com/oszuidwest/zwfm-encoder/releases) before upgrading: breaking changes and required actions are listed there.

## Configuration

Configuration is stored in `/etc/encoder/config.json`. For development, use the `-config` flag to specify a custom path, or place `config.json` next to the binary; a missing file is created with defaults. The file contains secrets, so keep it owner-only (`0600`).

Most settings are made in the web interface. These are not:

- `system`: `port`, `bind_address`, `username`, `password_hash`, `trusted_proxies`, `ffmpeg_path`
- `web`: `station_name`, `color_light`, `color_dark`
- `audio.input` for a [Dante-compatible input](#dante-compatible-input)
- `notifications.heartbeat.url` for the [heartbeat](#heartbeat)

Back up the file before editing and restart with `sudo systemctl restart encoder`. An invalid file keeps the encoder, and every stream, down; check `journalctl -u encoder` after the restart. Never change a recorder's `id`, `id_based_files` or `legacy_file_prefix`: file names and retention depend on them.

## Audio Input

Connect the audio output of your audio processor to the HiFiBerry input. The encoder captures 48 kHz, 16-bit stereo.

If the input fails or stops delivering audio (none within 30 seconds of opening, or a 10-second gap), the encoder reopens it every 3 to 15 seconds until it returns; streams and recorders keep running. The first failure logs `input_lost` and alerts every configured channel; `input_restored` follows after 10 seconds of steady audio, with the outage duration. An outage stays open across encoder stops and process restarts, so it alerts once. Changing the input in the web interface restarts the encoder. Recordings keep running through restarts and outages: after a second without audio a recorder writes silence, so a file keeps pace with the clock. Only stopping the encoder or the service ends them.

### Dante-compatible input

The encoder can receive two channels from a Dante-compatible transmitter directly, without Dante Virtual Soundcard. Set `audio.input` in `config.json`:

```json
{
  "audio": {
    "input": "dante://studio-tx/Program%20L/Program%20R?interface=eth0"
  }
}
```

The format is `dante://TRANSMITTER/LEFT_CHANNEL/RIGHT_CHANNEL?interface=INTERFACE`. Use the names shown in Dante Controller and percent-encode spaces and reserved characters in channel names. `interface` is optional; without it, discovery runs on all interfaces. Device names are up to 31 letters, digits and hyphens, without a port or `.local` suffix; channel names are up to 31 printable ASCII characters except `=`, `.` and `@`. The transmitter must run at 48 kHz with 16-, 24- or 32-bit PCM, and the network must pass mDNS multicast and the audio UDP traffic. Choosing another device in the web interface replaces the Dante input.

This receive-only implementation is experimental and unofficial: it is not Dante-certified and does not appear as a receiver in Dante Controller. See [`docs/dante-protocol.md`](docs/dante-protocol.md) for the protocol behavior and [`docs/dante-validation.md`](docs/dante-validation.md) for verification.

`Dante` is a trademark of Audinate Pty Ltd. This unofficial project is not affiliated with, authorized, sponsored, certified, approved, or endorsed by Audinate.

## Streaming

- **Push to server** (`mode: "caller"`): the encoder connects to a remote SRT listener. Requires FFmpeg with SRT support.
- **Local pull listener** (`mode: "listener"`): the encoder opens a local SRT port that clients pull from, for example `srt://<encoder-ip>:9000?mode=caller`. Each stream uses its own port and accepts up to 16 clients.

SRT encryption is optional: leave the password empty, or use 10-64 characters. Listener streams listen on all interfaces, so set a password or firewall their ports on untrusted networks.

| Codec | Encoder | Bitrate | Notes |
|-------|---------|---------|-------|
| MP3 | libmp3lame | 320 kbit/s (64-320 configurable) | - |
| Opus | libopus | 128 kbit/s (64-256 configurable) | MPEG-TS container, 10 ms frames |
| PCM | s302m | ~1.92 Mbit/s (16-bit) | MPEG-TS container, SMPTE 302M |

## Silence and Channel Imbalance Detection

Both detectors use hysteresis: an alarm starts after the duration and clears after the recovery time. Channel imbalance compares the left and right RMS levels and is only evaluated while at least one channel is above the silence threshold. Configure both under Settings -> Audio.

| Setting | Silence | Channel imbalance |
|---------|---------|-------------------|
| Threshold | -40 dB (-60 to -1) | 12 dB L/R difference (1 to 59) |
| Duration | 15 s (0.5 to 300) | 15 s (0.5 to 300) |
| Recovery | 5 s (0.5 to 60) | 5 s (0.5 to 60) |

With audio incident dumps enabled, recovery produces an MP3 with 15 seconds of context before and after the incident.

## Alerting

Configure notifications under Settings -> Notifications. Channels can be combined, each with per-event toggles:

| Channel | Events |
|---------|--------|
| Webhook (POST; redirects are rejected) | `silence_start`, `silence_end`, `audio_dump`, `channel_imbalance_start`, `channel_imbalance_end` |
| Email (Microsoft Graph) | Same as webhook |
| Zabbix (trapper items) | Same, except `audio_dump` |

Recovery events are sent as soon as recovery is confirmed; the MP3 follows as a separate `audio_dump_ready` event. Input loss and restore, overdue uploads and low recording storage always go to every configured channel, regardless of the toggles (on Zabbix, input events need the silence key and the others the upload key). Every event is also written to the [event log](#event-log).

### Microsoft 365 Email Setup

1. [Create an App Registration](https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade), then copy the **Client ID** and **Tenant ID**
2. Add API permissions: `Mail.Send` (required), `Application.Read.All` (optional, for secret expiry warnings)
3. Grant admin consent
4. Create a client secret, then copy the value immediately (won't be shown again)
5. [Create a shared mailbox](https://admin.exchange.microsoft.com/#/sharedmailboxes) as the sender (no license required)

The encoder warns when the secret expires within 30 days.

### Zabbix Setup

1. Import [`zabbix/template.xml`](zabbix/template.xml) in Zabbix (**Data collection** -> **Templates** -> **Import**)
2. Link the template to your encoder host
3. Configure in the encoder: server, port (default 10051), host name (must match Zabbix exactly) and the template's item keys `silence.alert`, `imbalance.alert` and `upload.alert`

The template has triggers for silence, input loss, channel imbalance, overdue uploads and low recording storage. Input events use the silence key; low storage uses `upload.alert.storage` (with a custom upload key, insert `.storage` before any parameters: `upload.alert[studio]` becomes `upload.alert.storage[studio]`). Re-import the template after upgrading to get new triggers.

## Recording

Each recorder writes to local disk, S3, or both, with retention cleanup. Configure recorders in the web interface.

- **Hourly** recorders start a new file on every whole hour with no gap between files, named after the local time, such as `<id-prefix>-YYYY-MM-DD-HH-00.<ext>`; the first file is named after its start minute. A repeated hour at the end of daylight saving time gets a `-2` suffix.
- **On-demand** recorders are started and stopped from the web interface or the [Recording API](#recording-api).

File names and S3 keys (`recordings/<id-prefix>/<filename>`) use the recorder ID, not its name, so renaming a recorder does not move its files. For a generated ID the prefix is that ID behind an `@`, such as `@recorder-<hex>`. Existing files are never overwritten.

Saving a recorder applies it right away. Enabling or disabling it and switching it to hourly take effect at once; a change of codec, bitrate, storage mode, local path, S3 endpoint or bucket starts a new file without a gap. If that file cannot start, recording continues in the current one and `status.error` shows why. After fixing the cause of a failed hourly recorder, save it to restart it; otherwise it retries at the next hour. Saving a failed on-demand recorder clears the error; start it again to record.

On Linux a recorder waits for an NTP-synchronized clock before its first file, because a Raspberry Pi has no real-time clock. Until then it shows `waiting for system clock synchronization` and hourly recorders make [`/ready`](#health-and-readiness) fail; streaming is not affected. The installer enables NTP; systemd-timesyncd and chrony (with Debian's default `rtcsync`) both work. A clock set by hand does not count, so the device needs a reachable NTP server.

### Recording API

On-demand recordings can be started and stopped via the REST API, for automation. Generate the API key and set the maximum recording duration (0 = no limit) under Settings -> Audio -> Recording API. The `recorder_id` is the recorder's `id` in `config.json` (`recording.recorders`), without the `@`; the web interface does not show it.

```bash
# Start recording
curl -X POST "http://<host>:8080/api/recordings/start?recorder_id=<recorder-id>" \
  -H "X-API-Key: <your-api-key>"

# Stop recording
curl -X POST "http://<host>:8080/api/recordings/stop?recorder_id=<recorder-id>" \
  -H "X-API-Key: <your-api-key>"
```

Only enabled on-demand recorders can be started, and only while the encoder runs; otherwise the API returns `400`. A wrong key returns `401`, and `503` means no key is set.

### Storage and S3

For local storage under the packaged service, use a directory under `/var/lib/encoder`; other paths need a drop-in that adds them to `ReadWritePaths=`. In S3-only mode, recordings wait in a spool under `/var/lib/encoder/recordings` until they are uploaded. The spool also holds the upload queue (`outbox/`), in every storage mode.

S3 storage must support conditional writes (`If-None-Match: *`), preserve user metadata and allow `HeadObject` (on AWS: `s3:GetObject`); the connection test checks metadata and `HeadObject`. Every finished recording is uploaded until it succeeds, also after a crash or restart, in local+S3 mode as well as S3-only, without duplicating objects. Failed attempts are retried after a minute, doubling up to an hour. Stopping a recorder or changing the audio input does not touch uploads: they continue in the background. A service stop interrupts a running upload (waiting at most a few seconds to abort a multipart upload), and the next start continues it. Add a bucket lifecycle rule that removes incomplete multipart uploads. A recording that has waited two hours sends `upload_overdue`; `upload_overdue_cleared` follows once none is overdue. A deleted recorder's recordings are still uploaded with the S3 settings they were recorded with; the encoder keeps a copy of those credentials in the spool, removed at the first start after the last of them is delivered. Changing a recorder's bucket or endpoint applies from the new file the save starts; changed credentials also reach recordings that wait.

Retention never deletes a recording that is not uploaded yet, or one that failed; it pauses entirely while the upload queue holds an entry that cannot be read. The encoder checks the spool and local recording paths every minute: below 1 GiB or 5% free (whichever is larger), or when a path cannot be checked, it sends `recording_storage_low`, and again by email and webhook when a path switches between low and unavailable; `recording_storage_restored` follows once an extra 1 GiB or 2.5% (whichever is larger) is free again, or on volumes under 3 GiB half the space above the warning level.

### Failed recordings

Each recording is in the upload queue from the moment its file is created. When the encoder detects that writing or finalizing failed, it reports `recorder_error`, marks the recording as failed and never uploads it, also after a restart. A recording that received no input audio is removed, also when the recorder filled it with silence (silence in the input itself is kept); a failed or crash-interrupted one is kept. After a crash, the next start uploads the interrupted file in both S3 modes. While a file is being written it is synced to disk every 30 seconds, so a power cut loses at most the audio after the last sync and what the encoder was still buffering.

Failed recordings are kept, and keep `/ready` failing, until you explicitly clear them. Stop the service, inspect the file and copy usable audio by hand, then delete the unwanted file, any `.failed` marker and all `<spool>/outbox/<ticket-id>.*.json` generations for that recording (match its `path` field), and restart. Missing files may be on unavailable storage: their tickets stay protected; S3 uploads retry with backoff and overdue alerting, while local-only recovery waits for another restart. Previously lost recordings are rechecked at startup; clear them the same way only after verifying the storage and salvaging any usable audio.

If only finalizing fails at an hourly rotation, the recorder keeps running and `status.error` shows the failure until the next file starts successfully. A failure while finalizing at a stop stays in `status.error` until the next successful start. Monitor `recorder_error` events, not only the recorder state. If recording itself fails, the recorder goes into error and retries at the next hour.

## Monitoring

### Heartbeat

With `notifications.heartbeat.url` set to a push monitor URL (for example an UptimeRobot heartbeat), the encoder sends a GET to it every minute while it runs, starting one minute after startup. The monitor goes down when the Pi, its network or the encoder is down; audio problems are covered by [alerting](#alerting). Redirects are not followed. The installer asks for the URL on a fresh install.

### Health and Readiness

Both endpoints are public (no login) and return JSON.

`GET /health` returns `200` while the encoder is running and FFmpeg is available, otherwise `503`. Stream, silence, imbalance and recorder problems do not affect it. During an input outage it returns `503` between reopen attempts, but it can return `200` while an open input delivers no audio. The body reports the encoder state, stream and recorder counts, uptime, and whether silence or imbalance is detected.

`GET /ready` returns `200` only when every component is ready, otherwise `503`:

| Component | Ready when |
|-----------|------------|
| `process` | Encoder running, FFmpeg available |
| `input` | Encoder running, audio received within the last 2 seconds, no open input outage |
| `streams` | Enabled push streams are stable (listener streams are excluded) |
| `silence` | No silence alarm |
| `channel_imbalance` | No imbalance alarm |
| `recorders` | Enabled hourly recorders running, no enabled recorder in error |
| `uploads` | No upload waiting longer than two hours, no failed or lost recording (a normal pending upload is fine) |
| `recording_storage` | All required paths checked within the last 3 minutes, none low or unavailable |

The response lists every component with `ok` and, where relevant, a `message` and `details`. The overall status is strict: one overdue upload, a failed recording or a planned silence makes it `503`. To page only on on-air problems, alert on `process`, `input`, `streams`, `silence` and `channel_imbalance` instead of the overall status.

### Event Log

Stream, audio (silence, channel imbalance, audio dump, input loss) and recording events are written as JSON Lines to `/var/log/encoder/<port>/encoder.jsonl` on Linux, and shown in the web interface. The log rotates at 50 MiB and keeps one previous file. See [docs/events.md](docs/events.md) for the complete event reference.

## Admin security

The encoder is an internal appliance: manage it by IP on a trusted LAN or through a VPN, and never forward its port from the public internet. It serves plain HTTP, so passwords and session cookies are only as safe as that network; put an HTTPS proxy in front when the path is not trusted. `/health` and `/ready` need no login.

### Network binding

`system.bind_address` defaults to `0.0.0.0` (all interfaces). Set a specific IPv4 or IPv6 address, without a port, to listen on one interface, or `127.0.0.1` for local-only access, then tunnel from your workstation with `ssh -L 8080:127.0.0.1:8080 user@raspberry-pi-ip` and open `http://127.0.0.1:8080`. Restart the encoder after changing it.

### Changing the admin password

The web interface has no password setting; the installer stores a hash in `system.password_hash`. Generate a new hash with the encoder binary, reading the password from stdin rather than the command line (Bash example):

```bash
read -r -s -p 'Admin password: ' encoder_password
printf '\n'
printf '%s' "$encoder_password" | /usr/local/bin/encoder -hash-password
unset encoder_password
```

Replace the `system.password_hash` value in `config.json` with the printed value (older configs: replace `system.password` with `password_hash`), restart the encoder and check that you can log in.

### HTTPS reverse proxy

For HTTPS, for example with Caddy on the same host, set `bind_address` to `127.0.0.1`, add `"trusted_proxies": ["127.0.0.1/32"]` to the `system` object, and use a Caddyfile like:

```caddyfile
encoder.example.com {
    header Strict-Transport-Security "max-age=31536000"
    reverse_proxy 127.0.0.1:8080
}
```

Keep the proxy restricted to your management network too; with a proxy on another host, bind the encoder to a private address and firewall its port to that proxy. The proxy must preserve the original `Host` and pass WebSockets. List only exact proxy addresses in `trusted_proxies`, never a client subnet; without it, all clients behind the proxy share one login limit.

### Login limits and API clients

Each client IPv4 address or IPv6 `/64` gets five login attempts per minute and each username ten, successful logins included. Beyond that the server answers `429` with `Retry-After: 60`; reload the login form before retrying. Login forms expire after ten minutes. Limits and sessions are reset when the service restarts, but a restart interrupts streams and recordings: wait 60 seconds instead. The username limit is shared by every client, also through an SSH tunnel, so someone guessing at it can lock you out for that minute: block their access and wait. A trusted proxy that forwards an unusable client address also gets `429`; check its `X-Forwarded-For`.

Session-based API clients must send `X-CSRF-Token` with every POST, PUT, PATCH and DELETE, using the token from `<meta name="csrf-token">` on the authenticated page; logout is `POST /logout`. The [Recording API](#recording-api) uses `X-API-Key` instead and needs no CSRF token.

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
