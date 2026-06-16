package streaming

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	ffmpegproc "github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/srtfanout"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestListenerPipeFanoutLateSubscriberReceivesMPEGTSBytes(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}

	tests := []struct {
		name    string
		codec   types.Codec
		encoder string
	}{
		{name: "opus", codec: types.CodecOpus, encoder: "libopus"},
		{name: "pcm", codec: types.CodecPCM, encoder: "s302m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !ffmpegHasEncoder(t, ffmpegPath, tt.encoder) {
				t.Skipf("ffmpeg encoder %s not available", tt.encoder)
			}

			port := freeUDPPort(t)
			fanout, err := srtfanout.NewServer(srtfanout.Config{
				StreamID: "listener-" + tt.name,
				BindHost: "127.0.0.1",
				Port:     port,
				Latency:  50 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			if err := fanout.Start(); err != nil {
				t.Fatalf("fanout Start() error = %v", err)
			}
			defer func() {
				fanout.Shutdown()
				if err := fanout.Wait(); err != nil {
					t.Fatalf("fanout Wait() error = %v", err)
				}
			}()

			stream := &types.Stream{
				ID:    "listener-" + tt.name,
				Mode:  types.StreamModeListener,
				Host:  "127.0.0.1",
				Port:  port,
				Codec: tt.codec,
			}
			result, err := ffmpegproc.StartProcessWithStdout(ffmpegPath, BuildListenerPipeArgs(stream))
			if err != nil {
				t.Fatalf("StartProcessWithStdout() error = %v", err)
			}

			stdoutDone := make(chan struct{})
			go func() {
				defer close(stdoutDone)
				buf := make([]byte, listenerStdoutBufferSize)
				stdout := result.Stdout()
				for {
					n, err := stdout.Read(buf)
					if n > 0 {
						fanout.Write(buf[:n])
					}
					if err != nil {
						return
					}
				}
			}()

			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				pcm := make([]byte, audio.BytesPerSecond/10)
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				deadline := time.After(3 * time.Second)
				for {
					select {
					case <-deadline:
						return
					case <-ticker.C:
						if _, err := result.WriteStdin(pcm); err != nil {
							return
						}
					}
				}
			}()
			defer func() {
				result.Cancel(errors.New("test stop"))
				result.CloseStdin()
				<-writerDone
				<-stdoutDone
				_ = result.Wait()
			}()

			time.Sleep(500 * time.Millisecond)
			conn := dialTestSubscriber(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			defer conn.Close()

			if n := readAtLeastOneSRTByte(t, conn); n == 0 {
				t.Fatal("late subscriber received no bytes")
			}
		})
	}
}

func ffmpegHasEncoder(t *testing.T, ffmpegPath, encoder string) bool {
	t.Helper()
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatalf("ffmpeg -encoders failed: %v", err)
	}
	return bytes.Contains(out, []byte(" "+encoder+" "))
}

func dialTestSubscriber(t *testing.T, addr string) srt.Conn {
	t.Helper()
	cfg := srt.DefaultConfig()
	cfg.ConnectionTimeout = time.Second
	cfg.Latency = 50 * time.Millisecond
	cfg.StreamId = "read:test"

	conn, err := srt.Dial("srt", addr, cfg)
	if err != nil {
		t.Fatalf("Dial(%s) error = %v", addr, err)
	}
	return conn
}

func readAtLeastOneSRTByte(t *testing.T, conn srt.Conn) int {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v", err)
	}
	if n == 0 {
		t.Logf("read returned %d bytes", n)
	}
	return n
}
