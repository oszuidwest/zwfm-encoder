//go:build windows

package audio

// buildFFmpegCaptureArgs constructs FFmpeg arguments for audio capture on Windows.
// Note: -nostdin is NOT used on Windows to allow graceful shutdown via 'q' command.
func buildFFmpegCaptureArgs(inputFormat, device string) []string {
	return []string{
		"-f", inputFormat,
		"-i", device,
		"-hide_banner",
		"-loglevel", "warning",
		"-vn",
		"-f", "s16le",
		"-ac", "2",
		"-ar", "48000",
		"pipe:1",
	}
}
