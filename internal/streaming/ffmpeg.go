// Package streaming manages FFmpeg streaming processes.
package streaming

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// callerSRTLatency is the SRT receiver buffer for caller-mode streams. It bounds
// how long the link can recover lost packets before audio must play out, so it
// dominates end-to-end latency. SRT negotiates the effective value as the
// maximum of both peers, so the receiving server must be configured at or below
// this value to benefit.
const callerSRTLatency = 2 * time.Second

// buildEncodeArgs returns the shared FFmpeg input and codec arguments used by
// both caller and listener encoding modes.
func buildEncodeArgs(stream *types.Stream) []string {
	codecArgs := types.BuildCodecArgs(stream.Codec, stream.Bitrate)
	args := ffmpeg.BaseInputArgs()
	args = append(args, "-hide_banner", "-loglevel", "warning", "-codec:a")
	return append(args, codecArgs...)
}

// BuildCallerArgs returns FFmpeg arguments for caller-mode SRT streaming.
func BuildCallerArgs(stream *types.Stream) []string {
	args := buildEncodeArgs(stream)
	return append(args, "-f", stream.Codec.Format(), BuildSRTURL(stream))
}

// BuildListenerPipeArgs returns FFmpeg arguments for listener-mode pipe encoding.
func BuildListenerPipeArgs(stream *types.Stream) []string {
	args := buildEncodeArgs(stream)
	format := stream.Codec.Format()
	if format == "mpegts" {
		args = append(args, "-pat_period", "0.1")
	}
	return append(args, "-f", format, "pipe:1")
}

// BuildSRTURL constructs a caller-mode SRT streaming URL.
func BuildSRTURL(stream *types.Stream) string {
	params := url.Values{}
	params.Set("pkt_size", "1316")
	params.Set("oheadbw", "100")
	params.Set("maxbw", "-1")
	params.Set("transtype", "live")
	params.Set("latency", strconv.FormatInt(callerSRTLatency.Microseconds(), 10))
	params.Set("mode", string(types.StreamModeCaller))
	params.Set("streamid", stream.StreamID)
	if stream.Password != "" {
		params.Set("passphrase", stream.Password)
		params.Set("pbkeylen", "16")
	}

	return fmt.Sprintf("srt://%s:%d?%s", stream.Host, stream.Port, params.Encode())
}
