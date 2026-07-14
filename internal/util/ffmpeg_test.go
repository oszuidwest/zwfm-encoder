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

func TestProbeFFmpegProtocolRetriesTimeoutsOnly(t *testing.T) {
	probeErr := errors.New("exec failed")
	type probeResult struct {
		output string
		err    error
	}
	tests := []struct {
		name string
		// results per attempt; the last entry repeats for further attempts
		results      []probeResult
		wantOK       bool
		wantErr      error
		wantAttempts int
	}{
		{
			name: "retries after timeout",
			results: []probeResult{
				{err: context.DeadlineExceeded},
				{output: "Input:\n  srt\n"},
			},
			wantOK:       true,
			wantAttempts: 2,
		},
		{
			name:         "errors after timeout exhaustion",
			results:      []probeResult{{err: context.DeadlineExceeded}},
			wantErr:      context.DeadlineExceeded,
			wantAttempts: ffmpegProtocolProbeAttempts,
		},
		{
			name:         "command failure not retried",
			results:      []probeResult{{err: probeErr}},
			wantErr:      probeErr,
			wantAttempts: 1,
		},
		{
			name:         "protocol unsupported not retried",
			results:      []probeResult{{output: "Input:\n  srtp\n"}},
			wantAttempts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := probeFFmpegProtocols
			t.Cleanup(func() { probeFFmpegProtocols = orig })
			attempts := 0
			probeFFmpegProtocols = func(string) ([]byte, error) {
				result := tt.results[min(attempts, len(tt.results)-1)]
				attempts++
				return []byte(result.output), result.err
			}

			ok, err := ProbeFFmpegProtocol("ffmpeg", "srt")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ProbeFFmpegProtocol() error = %v, want %v", err, tt.wantErr)
			}
			if ok != tt.wantOK {
				t.Fatalf("ProbeFFmpegProtocol() ok = %t, want %t", ok, tt.wantOK)
			}
			if attempts != tt.wantAttempts {
				t.Fatalf("probe attempts = %d, want %d", attempts, tt.wantAttempts)
			}
		})
	}
}
