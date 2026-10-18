# Independent Inferno interoperability test

From the repository root, with Go, Git and Docker running:

```sh
bash tests/dante-inferno/run.sh
```

The script builds unmodified [Inferno](https://github.com/teodly/inferno/tree/9767558e0905c9e9c960f5a60e050fbdc38300b7) in release mode, its upstream fake clock, and the current receiver test. It creates an isolated Docker network without publishing ports and removes its containers and network on exit; images, source, audio logs and the compiled test remain in the printed artifact directory.

FFmpeg supplies stereo tones (440 Hz left, 880 Hz right, amplitude 0.25) to Inferno's ALSA plugin. The sender uses Inferno's supported 50 ms transmit-latency setting: its default 10 ms can skip source samples under Docker VM load (inspect `inferno.log` for underruns). A test-only adapter (`relay.py`) forwards Inferno's multicast mDNS replies from port 5353 to the receiver's query port. What this does and does not verify is described in [docs/dante-validation.md](../../docs/dante-validation.md).
