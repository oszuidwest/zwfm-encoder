// Package streaming manages FFmpeg streaming processes.
package streaming

import (
	"fmt"
	"net/url"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// BuildFFmpegArgs returns FFmpeg arguments for streaming.
func BuildFFmpegArgs(stream *types.Stream) []string {
	codecArgs := types.BuildCodecArgs(stream.Codec, stream.Bitrate)
	format := stream.Codec.Format()
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
	params.Set("transtype", "live")

	host := stream.Host
	switch stream.ModeOrDefault() {
	case types.StreamModeListener:
		host = stream.ListenerBindHost()
		params.Set("latency", "300000")
		params.Set("listen_timeout", "-1")
		params.Set("mode", string(types.StreamModeListener))
	default:
		params.Set("latency", "10000000")
		params.Set("mode", string(types.StreamModeCaller))
		params.Set("streamid", stream.StreamID)
	}
	if stream.Password != "" {
		params.Set("passphrase", stream.Password)
		params.Set("pbkeylen", "16")
	}

	return fmt.Sprintf("srt://%s:%d?%s", host, stream.Port, params.Encode())
}
