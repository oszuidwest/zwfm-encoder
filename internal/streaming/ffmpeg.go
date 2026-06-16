// Package streaming manages FFmpeg streaming processes.
package streaming

import (
	"fmt"
	"net/url"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// BuildCallerArgs returns FFmpeg arguments for caller-mode SRT streaming.
func BuildCallerArgs(stream *types.Stream) []string {
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

// BuildListenerPipeArgs returns FFmpeg arguments for listener-mode pipe encoding.
func BuildListenerPipeArgs(stream *types.Stream) []string {
	codecArgs := types.BuildCodecArgs(stream.Codec, stream.Bitrate)
	format := stream.Codec.Format()

	args := ffmpeg.BaseInputArgs()
	args = append(args, "-hide_banner", "-loglevel", "warning", "-codec:a")
	args = append(args, codecArgs...)
	if format == "mpegts" {
		args = append(args, "-pat_period", "0.1")
	}
	args = append(args, "-f", format, "pipe:1")
	return args
}

// BuildSRTURL constructs a caller-mode SRT streaming URL.
func BuildSRTURL(stream *types.Stream) string {
	params := url.Values{}
	params.Set("pkt_size", "1316")
	params.Set("oheadbw", "100")
	params.Set("maxbw", "-1")
	params.Set("transtype", "live")
	params.Set("latency", "10000000")
	params.Set("mode", string(types.StreamModeCaller))
	params.Set("streamid", stream.StreamID)
	if stream.Password != "" {
		params.Set("passphrase", stream.Password)
		params.Set("pbkeylen", "16")
	}

	return fmt.Sprintf("srt://%s:%d?%s", stream.Host, stream.Port, params.Encode())
}
