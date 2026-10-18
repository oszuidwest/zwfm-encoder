//go:build integration

package recording

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestRealFFmpegWritesReservedOutputForEveryCodec(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal("integration test requires ffmpeg on PATH")
	}
	for _, codec := range []types.Codec{types.CodecPCM, types.CodecMP3, types.CodecOpus} {
		t.Run(string(codec), func(t *testing.T) {
			cfg := localOnDemandRecorder(t, "real-ffmpeg")
			cfg.Codec = codec
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, FFmpegPath: ffmpegPath})
			path := startCaptureTestRecorder(t, r)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("output was not reserved before audio: %v, %v", info, err)
			}
			r.WriteAudio(bytes.Repeat([]byte{0, 1, 0, 2}, 48000))
			if err := r.Stop(); err != nil {
				t.Fatal(err)
			}
			final, err := os.Stat(path)
			if err != nil || final.Size() == 0 || !os.SameFile(info, final) {
				t.Fatalf("FFmpeg did not write its reserved inode: %v, %v", final, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			decoded, err := util.CommandContext(ctx, ffmpegPath, "-v", "error", "-i", path,
				"-f", "s16le", "-acodec", "pcm_s16le", "pipe:1").Output()
			if err != nil || len(decoded) < 48000*4 {
				t.Fatalf("reserved recording cannot be decoded: %d bytes, %v", len(decoded), err)
			}
		})
	}
}
