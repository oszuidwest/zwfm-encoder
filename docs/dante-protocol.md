# Functional specification: receive-only Dante-compatible stereo input

The receiver in `internal/dante` was implemented independently from this
specification. Dante is a trademark of Audinate Pty Ltd; this project is not
affiliated with or endorsed by Audinate.

This document specifies a Go package `dante` that receives two audio channels
from a Dante-compatible network audio transmitter and exposes them as a PCM
byte stream. It describes observable protocol behavior (wire formats,
constants, message sequences) and the required API. It contains no source
code. All wire facts below have been verified against a Dante Virtual
Soundcard transmitter on a local network.

All multi-byte integers on the wire are big-endian unless stated otherwise.

## 1. Required Go API

Package name: `dante`. Allowed imports: the Go standard library and
`golang.org/x/net` (already in `go.mod`). Go 1.27.

```
func IsInput(input string) bool
func Open(ctx context.Context, input string) (*Receiver, error)

type Receiver struct { /* unexported */ }
func (r *Receiver) Read(p []byte) (int, error)
func (r *Receiver) Wait() error
func (r *Receiver) Close() error
```

- `IsInput` reports whether `input` parses as a URL whose scheme is `dante`
  (case-insensitive).
- `Open` parses the input (section 2), discovers both channels (section 3),
  requests a flow (section 4), starts reception (section 5) in a background
  goroutine and returns. Any failure before returning must release every
  socket it opened. `Open` must honor `ctx` during discovery and flow setup.
- Cancelling the `ctx` passed to `Open` (after `Open` returned) must stop
  reception and close the receiver, exactly as `Close` does.
- `Read` returns interleaved stereo PCM: 48000 Hz, signed 16-bit
  little-endian, left sample then right sample. After reception ends, `Read`
  returns `io.EOF` for a clean stop (Close or context cancellation) or the
  terminal error otherwise.
- `Wait` blocks until reception has fully ended (including the stop-flow
  message of section 4.4) and returns `nil` for a clean stop or the terminal
  error.
- `Close` is idempotent, safe to call concurrently with `Read` and `Wait`,
  and unblocks any pending `Read`.
- The consumer calls `Read` continuously from its own goroutine.

## 2. Input URI

```
dante://<transmitter>/<left channel>/<right channel>[?interface=<name>]
```

- `<transmitter>` is the URL host and must be non-empty.
- The path must have exactly two non-empty segments. Each segment is
  percent-decoded (so `Program%20L` means `Program L` and `A%2FB` means
  `A/B`). Split the escaped path before decoding.
- `interface` (optional) names the local network interface to use.
- Any other shape is an error with a helpful message showing the format.

## 3. Discovery (DNS-SD over multicast DNS)

Each transmitter channel is advertised as its own DNS-SD service instance:

- Service type: `_netaudio-chan._udp.local.`
- Instance name: `<channel name>@<transmitter name>`, for example
  `01@studio-tx`. The full record name is
  `01@studio-tx._netaudio-chan._udp.local.`
- SRV record: the port is the transmitter's control port (observed: 4455).
- TXT record: `key=value` strings. Relevant keys:

| Key     | Meaning                                                     | Example   |
|---------|-------------------------------------------------------------|-----------|
| `id`    | Channel number used in flow requests (decimal or `0x` hex)  | `1`       |
| `rate`  | Sample rate in Hz                                           | `48000`   |
| `enc`   | Bits per sample. Some devices publish `en` instead; accept either, prefer `enc` | `24` |
| `nchan` | Maximum number of channels in one flow                      | `2`       |
| `fpp`   | Frames per packet as `max,min`                              | `32,32`   |

Other keys are present and must be ignored. A full observed TXT record:
`txtvers=2 dbcp1=0x1200 id=1 dbcp=0x1004 rate=48000 en=24 pcm=3 e enc=24
latency_ns=10000000 fpp=32,32 nchan=2 at2`. Note that items without `=` exist.

Discovery requirements:

- Use RFC 6762 section 5.1 one-shot queries: send a standard DNS query
  (questions for SRV and TXT of the full instance name, class IN) to
  224.0.0.251:5353 from an ephemeral UDP port. Responders answer by unicast to
  that port. Do not bind port 5353 and do not join the multicast group.
- If an interface is configured, send the query on that interface only.
  Otherwise send it on every interface that is up, multicast-capable and not
  loopback.
- Re-send the query every second; give up after 4 seconds per channel.
- Responses frequently contain records for other instances of the same
  service (observed). Only use SRV and TXT records whose owner name equals
  the requested full name (case-insensitive). Look in both the answer and the
  additional sections. SRV and TXT may arrive in different datagrams.
- The transmitter's IPv4 address is the source address of the response.
- Hosts with several network interfaces (for example a transmitter on a
  primary and a secondary network, or a host on both wired and wireless)
  answer from several source addresses. Observed: without an interface
  configured, the two channels were sometimes resolved from different
  addresses of the same transmitter. Therefore resolve the left channel
  first, then resolve the right channel accepting only responses whose
  source address equals the left channel's transmitter address.
- Parse all TXT values defensively. A malformed TXT record for the requested
  name is ignored (keep waiting; it may come from another host); if the
  lookup times out, report the last parse error. `id` must fit in 16 bits.

Validation after resolving both channels:

- Both channels must have the same transmitter address and port, the same
  bits per sample, sample rate and `nchan`; otherwise error.
- Sample rate must be 48000; `nchan` must be at least 2; bits per sample must
  be 16, 24 or 32.

Local address selection:

- With `interface`: the first non-loopback IPv4 address of that interface;
  the interface must be up and multicast-capable.
- Without: the source address the operating system would use to reach the
  transmitter (for example via the local address of a connected UDP socket).

## 4. Flow control protocol (UDP)

Control messages are exchanged between a UDP socket bound to the local IPv4
address (ephemeral port) and the transmitter's IP and control port.

### 4.1 Header (10 bytes, both directions)

| Offset | Size | Field                                                           |
|--------|------|-----------------------------------------------------------------|
| 0      | 2    | Start code: always `0x1102` in requests                         |
| 2      | 2    | Total message length in bytes, including this header            |
| 4      | 2    | Sequence number chosen by the requester; echoed in the response |
| 6      | 2    | Opcode; echoed in the response                                  |
| 8      | 2    | Status: `0x0000` in requests. In responses `0x0001` = success, anything else is an error code |

A response belongs to a request when sequence and opcode match. Ignore other
datagrams and datagrams shorter than the header or whose length field is
smaller than 10 or larger than the datagram. Wait at most 3 seconds (and never
past the context deadline) for the response. Observed error code: `0x0314`
when the transmitter itself has no usable network.

### 4.2 Request flow (opcode `0x0100`)

Let N be the number of channels (2 here) and H = 10 (header size). The body
directly follows the header. Offsets below are relative to the start of the
body; "absolute offset" means relative to the start of the whole message.

| Body offset | Size | Value                                                             |
|-------------|------|-------------------------------------------------------------------|
| 0           | 2    | Absolute offset of the string area (= H + 38 + 2N)                 |
| 2           | 4    | Sample rate (48000)                                                |
| 6           | 4    | Bits per sample (from the TXT record)                              |
| 10          | 2    | `0x0001`                                                           |
| 12          | 2    | N                                                                  |
| 14          | 2    | Absolute offset of the address block (see string area)            |
| 16          | 2N   | Channel `id` values, one u16 each, in the order they should appear in media packets (left, then right) |
| 16 + 2N     | 2    | `0x001c + 2N`                                                      |
| 18 + 2N     | 2    | `0x0a00`                                                           |
| 20 + 2N     | 2    | `0x0002`                                                           |
| 22 + 2N     | 2    | Frames per packet requested (section 4.3)                          |
| 24 + 2N     | 2    | Absolute offset of the flow name string                            |
| 26 + 2N     | 12   | Zero bytes                                                         |

The string area starts at absolute offset H + 38 + 2N and contains, in order:

1. Receiver name, ASCII, followed by one zero byte. Use the local host name
   (trimmed, at most 31 bytes); fall back to `ZWFM Encoder` if unavailable.
2. Flow name, ASCII, followed by one zero byte. Use `encoder_1`.
3. Zero bytes until the absolute offset is a multiple of 8.
4. The address block (its absolute offset is what body offset 14 refers to):
   u16 `0x0802`, u16 local media UDP port, 4 bytes local IPv4 address.

The whole message must not exceed 65535 bytes; reject otherwise.

On success the response body's first 6 bytes are an opaque flow handle. A
shorter body is an error.

### 4.3 Frames per packet

Choose fpp = min(fpp max, floor(1400 / (N * bytes per sample))). If that is
below fpp min, fail with an error explaining that the transmitter's minimum
packet size exceeds the MTU budget.

### 4.4 Stop flow (opcode `0x0101`)

Body: the 6-byte flow handle. Send it when reception ends for any reason, use
a sequence number different from the request, and wait at most 1 second for
the response. Errors are ignored.

## 5. Media reception

Open the media socket (UDP, bound to the local IPv4 address, ephemeral port)
before requesting the flow; its port goes into the address block.

### 5.1 Media datagram

| Offset | Size | Field                                                      |
|--------|------|------------------------------------------------------------|
| 0      | 1    | Unspecified, ignore                                        |
| 1      | 4    | Seconds                                                    |
| 5      | 4    | Sample offset within the second                            |
| 9      | ...  | Payload                                                    |

The stream position (in frames) of the first frame of the payload is
`seconds * 48000 + sample offset`. The payload is a whole number of frames;
each frame has N samples in the requested channel order; each sample is
bits/8 bytes, signed, big-endian and left-justified (the first byte is the
most significant). Conversion to 16-bit output takes the first two bytes of
each sample (then write them little-endian).

A datagram shorter than 9 bytes or whose payload is not a whole number of
frames is malformed. Malformed datagrams must be dropped, never fatal (any
host can send datagrams to the port).

### 5.2 Keepalive

At least every 250 ms, once media has been received, send a 2-byte UDP
datagram `0x13 0x37` from the media socket to the source address of the most
recent media datagram. Without keepalives the transmitter stops the flow.

### 5.3 Ordering, loss and timeouts

- The first valid datagram defines the expected position.
- Datagrams whose position is below the expected position are late or
  duplicate: drop them.
- Out-of-order datagrams are buffered and released in position order.
- If the expected position is still missing 4 ms after buffered data is
  waiting, write silence for the missing frames and continue with the
  earliest buffered datagram. Buffered entries that start below the expected
  position (overlaps) must be discarded so they can never block progress.
- Gap handling must run after every received datagram as well as when the
  socket is idle; packets can arrive faster than any idle timeout.
- A gap larger than 48000 frames (1 s) is a terminal error.
- If no valid media arrives for 5 seconds, reception ends with a timeout
  error.
- Write output in batches of about 10 ms (1920 bytes) to limit wake-ups of
  the reader; flush remaining data when reception ends.

## 6. Quality requirements

- Never panic on network input; bounds-check every field before use.
- Every exported identifier has a doc comment; code is gofmt-formatted.
- Table-driven unit tests with `t.Parallel()` for: URI parsing, TXT parsing,
  selecting only the requested instance from a DNS response that also
  contains another instance, flow request encoding (check every field for
  N = 2), control response matching and error codes (fake transmitter on
  127.0.0.1), media decoding for 16/24/32-bit, malformed media being dropped,
  reordering, gap filling, overlap discarding, and Close/cancel semantics.
- Keep the implementation small and readable: prefer the standard library,
  avoid unnecessary abstractions.
