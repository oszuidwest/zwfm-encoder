// Package output manages FFmpeg output processes for streaming.
package output

import (
	"fmt"
	"net/url"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// BuildFFmpegArgs returns the FFmpeg arguments for an output.
func BuildFFmpegArgs(output *types.Output) []string {
	codecArgs := output.CodecArgs()
	format := output.Format()
	srtURL := BuildSRTURL(output)

	// Start with base input args, add output-specific flags
	args := ffmpeg.BaseInputArgs()
	args = append(args, "-hide_banner", "-loglevel", "warning", "-codec:a")
	args = append(args, codecArgs...)
	args = append(args, "-f", format, srtURL)
	return args
}

// BuildSRTURL constructs the SRT URL for an output.
func BuildSRTURL(output *types.Output) string {
	params := url.Values{}
	params.Set("pkt_size", "1316")
	params.Set("oheadbw", "100")
	params.Set("maxbw", "-1")
	params.Set("latency", "10000000")
	params.Set("mode", "caller")
	params.Set("transtype", "live")
	params.Set("streamid", output.StreamID)
	params.Set("passphrase", output.Password)

	return fmt.Sprintf("srt://%s:%d?%s", output.Host, output.Port, params.Encode())
}
