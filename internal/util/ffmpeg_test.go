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
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing-ffmpeg")
	ok, err := ProbeFFmpegProtocol(missing, "srt")
	if err == nil {
		t.Fatal("ProbeFFmpegProtocol() error = nil, want probe error")
	}
	if ok {
		t.Fatal("ProbeFFmpegProtocol() ok = true, want false on probe error")
	}
}

func TestProbeFFmpegProtocolRetriesAfterTimeout(t *testing.T) {
	t.Parallel()
	attempts := 0
	probe := func(string) ([]byte, error) {
		attempts++
		if attempts == 1 {
			return nil, context.DeadlineExceeded
		}
		return []byte("Input:\n  srt\n"), nil
	}

	ok, err := probeFFmpegProtocol("ffmpeg", "srt", probe)
	if err != nil {
		t.Fatalf("probeFFmpegProtocol() error = %v, want nil", err)
	}
	if !ok {
		t.Fatal("probeFFmpegProtocol() ok = false, want true after retry")
	}
	if attempts != 2 {
		t.Fatalf("probe attempts = %d, want 2", attempts)
	}
}

func TestProbeFFmpegProtocolReturnsErrorAfterTimeouts(t *testing.T) {
	t.Parallel()
	attempts := 0
	probe := func(string) ([]byte, error) {
		attempts++
		return nil, context.DeadlineExceeded
	}

	ok, err := probeFFmpegProtocol("ffmpeg", "srt", probe)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probeFFmpegProtocol() error = %v, want context deadline exceeded", err)
	}
	if ok {
		t.Fatal("probeFFmpegProtocol() ok = true, want false after timeout exhaustion")
	}
	if attempts != ffmpegProtocolProbeAttempts {
		t.Fatalf("probe attempts = %d, want %d", attempts, ffmpegProtocolProbeAttempts)
	}
}

func TestProbeFFmpegProtocolDoesNotRetryDefinitiveResult(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("exec failed")
	tests := []struct {
		name    string
		output  string
		err     error
		wantOK  bool
		wantErr error
	}{
		{
			name:    "command failure",
			err:     probeErr,
			wantErr: probeErr,
		},
		{
			name:   "protocol unsupported",
			output: "Input:\n  srtp\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			probe := func(string) ([]byte, error) {
				attempts++
				return []byte(tt.output), tt.err
			}

			ok, err := probeFFmpegProtocol("ffmpeg", "srt", probe)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("probeFFmpegProtocol() error = %v, want %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("probeFFmpegProtocol() ok = %t, want %t", ok, tt.wantOK)
			}
			if attempts != 1 {
				t.Fatalf("probe attempts = %d, want 1", attempts)
			}
		})
	}
}
