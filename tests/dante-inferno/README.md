# Independent Inferno interoperability test

From the repository root, with Go, Git and Docker running:

```sh
bash tests/dante-inferno/run.sh
```

The script builds unmodified [Inferno](https://github.com/teodly/inferno/tree/9767558e0905c9e9c960f5a60e050fbdc38300b7) in release mode, its upstream fake clock, and the current receiver tests. It creates an isolated Docker network without publishing ports. Its temporary sender and network are removed on exit; images, source, audio logs and the compiled test remain in the printed artifact directory.

FFmpeg supplies stereo tones (440 Hz left, 880 Hz right, amplitude 0.25) to Inferno's ALSA plugin. The sender uses Inferno's supported 50 ms transmit-latency setting to accommodate Docker scheduling; its default 10 ms setting can skip source samples under VM load. The Go test asserts channel frequency, level, real-time pacing, cancellation, reopening, channel swapping and automatic interface selection. It also runs normal protocol tests separately with `go test -race ./internal/dante`.

## Discovery adapter and limits

This Inferno revision answers ephemeral-port mDNS queries via multicast, whereas [RFC 6762 section 6.7](https://www.rfc-editor.org/rfc/rfc6762.html#section-6.7) requires a unicast reply to the query source port. A test-only adapter forwards Inferno's original response bytes from port 5353 to that ephemeral port. It changes only delivery, and never handles flow control or audio. Production discovery validation is unchanged. Successful runs therefore prove independent flow/audio compatibility with this adapter, not direct Inferno discovery compatibility.

The clock is synthetic; this does not verify PTP interoperability or Audinate certification. The upstream sender advertises 24-bit media. Synthetic tests cover 16/24/32-bit decoding and malformed/control/loss scenarios. Tone tolerances permit small scheduling gaps in Docker; inspect `inferno.log` for sender underruns. Passing this test is not a dropout-free hardware guarantee.
