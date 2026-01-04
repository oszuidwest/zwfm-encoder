//go:build !linux

package audio

// buildFFmpegCaptureArgs constructs FFmpeg arguments for audio capture.
func buildFFmpegCaptureArgs(inputFormat, device string) []string {
	return []string{
		"-f", inputFormat,
		"-i", device,
		"-nostdin",
		"-hide_banner",
		"-loglevel", "warning",
		"-vn",
		"-f", "s16le",
		"-ac", "2",
		"-ar", "48000",
		"pipe:1",
	}
}
