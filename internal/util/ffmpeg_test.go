package util

import "testing"

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
