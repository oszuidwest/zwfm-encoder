package util

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestFFmpegProtocolListContainsExactToken(t *testing.T) {
	t.Parallel()
	output := `
Supported file protocols:
Input:
  file
  srtp
  tcp
Output:
  file
  srt
  udp
`
	if !FFmpegProtocolListContains(output, "srt") {
		t.Fatal("FFmpegProtocolListContains() = false, want true for exact srt token")
	}
	if FFmpegProtocolListContains(output, "rt") {
		t.Fatal("FFmpegProtocolListContains() = true, want false for partial token")
	}
}
func TestFFmpegProtocolListContainsRejectsOnlySRTP(t *testing.T) {
	t.Parallel()
	output := `
Supported file protocols:
Input:
  srtp
Output:
  srtp
`
	if FFmpegProtocolListContains(output, "srt") {
		t.Fatal("FFmpegProtocolListContains() = true, want false when only srtp is present")
	}
}
func TestProbeFFmpegProtocolReturnsProbeError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-ffmpeg")
	ok, err := ProbeFFmpegProtocol(missing, "srt")
	if err == nil {
		t.Fatal("ProbeFFmpegProtocol() error = nil, want probe error")
	}
	if ok {
		t.Fatal("ProbeFFmpegProtocol() ok = true, want false on probe error")
	}
}

func TestProbeFFmpegProtocolReturnsProbeResult(t *testing.T) {
	probeErr := errors.New("exec failed")
	tests := []struct {
		name     string
		output   string
		probeErr error
		wantOK   bool
		wantErr  error
	}{
		{
			name:   "supported protocol",
			output: "Input:\n  srt\n",
			wantOK: true,
		},
		{
			name:     "timeout",
			probeErr: context.DeadlineExceeded,
			wantErr:  context.DeadlineExceeded,
		},
		{
			name:     "command failure",
			probeErr: probeErr,
			wantErr:  probeErr,
		},
		{
			name:   "protocol unsupported",
			output: "Input:\n  srtp\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := probeFFmpegProtocols
			t.Cleanup(func() { probeFFmpegProtocols = orig })
			attempts := 0
			probeFFmpegProtocols = func(string) ([]byte, error) {
				attempts++
				return []byte(tt.output), tt.probeErr
			}

			ok, err := ProbeFFmpegProtocol("ffmpeg", "srt")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ProbeFFmpegProtocol() error = %v, want %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("ProbeFFmpegProtocol() ok = %t, want %t", ok, tt.wantOK)
			}
			if attempts != 1 {
				t.Fatalf("probe attempts = %d, want 1", attempts)
			}
		})
	}
}
