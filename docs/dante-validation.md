# Dante-compatible receiver validation

This validates the receive-only 48 kHz stereo subset described in [dante-protocol.md](dante-protocol.md). Public Audinate documentation does not supply a complete wire-protocol specification. Inferno is an independent, unofficial implementation, not an Audinate conformance oracle. This project does not claim certification.

| Area | Reference and verification |
| --- | --- |
| Device and channel names | [Audinate naming rules](https://dev.audinate.com/GA/dante-controller/userguide/webhelp/content/device_and_channel_names.htm); URI tests cover length, ASCII, reserved characters, escaping and malformed inputs. |
| Discovery transport and origin | [RFC 6762 §§5.1, 6.7, 11](https://www.rfc-editor.org/rfc/rfc6762.html); packet tests cover source port, receiving interface/subnet, DNS headers, classes and requested owner names. Native UDP ancillary-data tests also run on Windows CI. |
| TXT parsing | [RFC 6763 §6.4](https://www.rfc-editor.org/rfc/rfc6763.html#section-6.4); table tests cover key spelling, duplicates, encoding fallback, number formats and invalid values. |
| Flow creation and deletion | [Inferno flow client](https://github.com/teodly/inferno/blob/9767558e0905c9e9c960f5a60e050fbdc38300b7/inferno_aoip/src/protocol/flows_control.rs) and independent transmitter; field-level unit tests plus actual `Open`/`Close` tests check channel IDs, offsets, sample rate, bit depth, fpp, control responses and handles. |
| Media samples and timestamps | [Inferno transmitter](https://github.com/teodly/inferno/blob/9767558e0905c9e9c960f5a60e050fbdc38300b7/inferno_aoip/src/device_server/flows_tx.rs); tests cover network PCM to stereo S16LE at 16/24/32 bits, timestamps, malformed packets, reordering, overlaps and loss. |
| Keepalives and lifecycle | Independent Inferno accepts the media-socket keepalive; deterministic tests cover missing media, cancellation, blocked writes, setup failures, concurrent Close, Read/Wait errors and stop-flow cleanup. |

The 4 ms reorder wait, one-second maximum filled gap, five-second media timeout and batching sizes are receiver policies. They are not presented as proprietary Dante requirements.

## Independent live test

Run [the Docker harness](../tests/dante-inferno/README.md) on macOS or Linux. It builds pinned Inferno revision `9767558e0905c9e9c960f5a60e050fbdc38300b7` in release mode and tests actual network flow/audio behavior, including reversed channels and reopening.

The combined proposed fixes passed on Docker Desktop Linux arm64 on macOS on 2026-10-18. Stereo, reopen, reversed channels and automatic interface selection each received six seconds of audio and shut down successfully. Measured tones were 440.2–440.8 Hz and 880.6–881.2 Hz at -15.05 dBFS RMS. An initial run failed the existing tone tolerance while Inferno logged source samples being skipped at its default 10 ms transmit latency. Configuring the sender's supported 50 ms latency removed those reported skips in the final run; receiver assertions were unchanged. This short run does not establish long-duration or physical-network reliability.

Inferno's multicast-only response behavior requires a test-only mDNS destination adapter for this receiver's RFC 6762 legacy queries. The adapter preserves DNS bytes and source port 5353; all control and media packets travel directly between the implementations. Direct Inferno discovery, physical Dante hardware, PTP interoperability, long-duration reliability, redundancy, AES67 and certification are not verified by this harness. Hardware acceptance testing remains necessary before changing a production audio path.
