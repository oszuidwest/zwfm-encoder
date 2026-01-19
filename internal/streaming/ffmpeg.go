// Package streaming provides FFmpeg process management for SRT output streams.
package streaming

import (
	"fmt"
	"net/url"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// BuildFFmpegArgs returns FFmpeg arguments for streaming.
func BuildFFmpegArgs(stream *types.Stream) []string {
	codecArgs := stream.CodecArgs()
	format := stream.Format()
	srtURL := BuildSRTURL(stream)

	// Start with base input args, add stream-specific flags
	args := ffmpeg.BaseInputArgs()
	args = append(args, "-hide_banner", "-loglevel", "warning", "-codec:a")
	args = append(args, codecArgs...)
	args = append(args, "-f", format, srtURL)
	return args
}

// BuildSRTURL constructs an SRT streaming URL.
func BuildSRTURL(stream *types.Stream) string {
	params := url.Values{}
	params.Set("pkt_size", "1316")
	params.Set("oheadbw", "100")
	params.Set("maxbw", "-1")
	params.Set("latency", "10000000")
	params.Set("mode", "caller")
	params.Set("transtype", "live")
	params.Set("streamid", stream.StreamID)
	params.Set("passphrase", stream.Password)

	return fmt.Sprintf("srt://%s:%d?%s", stream.Host, stream.Port, params.Encode())
}
