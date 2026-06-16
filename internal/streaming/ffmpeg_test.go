package streaming

import (
	"net/url"
	"slices"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestBuildSRTURLModeAware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stream     types.Stream
		wantHost   string
		wantParams map[string]string
		absent     []string
	}{
		{
			name: "caller keeps existing latency and stream id",
			stream: types.Stream{
				Host:     "stream.example.com",
				Port:     9000,
				StreamID: "studio",
				Codec:    types.CodecMP3,
			},
			wantHost: "stream.example.com:9000",
			wantParams: map[string]string{
				"latency":   "10000000",
				"mode":      "caller",
				"streamid":  "studio",
				"transtype": "live",
			},
			absent: []string{"listen_timeout", "passphrase", "pbkeylen"},
		},
		{
			name: "caller with password sets pbkeylen",
			stream: types.Stream{
				Host:     "stream.example.com",
				Port:     9000,
				Password: "1234567890",
				StreamID: "studio",
				Codec:    types.CodecMP3,
			},
			wantHost: "stream.example.com:9000",
			wantParams: map[string]string{
				"mode":       "caller",
				"passphrase": "1234567890",
				"pbkeylen":   "16",
			},
		},
		{
			name: "listener empty host binds all interfaces",
			stream: types.Stream{
				Mode:  types.StreamModeListener,
				Port:  9000,
				Codec: types.CodecMP3,
			},
			wantHost: "0.0.0.0:9000",
			wantParams: map[string]string{
				"latency":        "300000",
				"listen_timeout": "-1",
				"mode":           "listener",
				"transtype":      "live",
			},
			absent: []string{"streamid", "passphrase", "pbkeylen"},
		},
		{
			name: "listener with explicit bind and password",
			stream: types.Stream{
				Mode:     types.StreamModeListener,
				Host:     "192.0.2.10",
				Port:     9000,
				Password: "1234567890",
				Codec:    types.CodecMP3,
			},
			wantHost: "192.0.2.10:9000",
			wantParams: map[string]string{
				"mode":       "listener",
				"passphrase": "1234567890",
				"pbkeylen":   "16",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			u, err := url.Parse(BuildSRTURL(&tt.stream))
			if err != nil {
				t.Fatalf("Parse(BuildSRTURL()) error = %v", err)
			}
			if u.Scheme != "srt" {
				t.Fatalf("scheme = %q, want srt", u.Scheme)
			}
			if u.Host != tt.wantHost {
				t.Fatalf("host = %q, want %q", u.Host, tt.wantHost)
			}
			params := u.Query()
			for key, want := range tt.wantParams {
				if got := params.Get(key); got != want {
					t.Fatalf("query %q = %q, want %q; url=%s", key, got, want, u.String())
				}
			}
			for _, key := range tt.absent {
				if _, exists := params[key]; exists {
					t.Fatalf("query %q present in %s, want absent", key, u.String())
				}
			}
		})
	}
}

func TestBuildFFmpegArgsUsesCallerSRTOutput(t *testing.T) {
	t.Parallel()

	stream := &types.Stream{
		Host:     "stream.example.com",
		Port:     9000,
		StreamID: "studio",
		Codec:    types.CodecMP3,
	}

	args := BuildFFmpegArgs(stream)
	if slices.Contains(args, "pipe:1") {
		t.Fatalf("caller args contain pipe:1: %v", args)
	}
	if got := args[len(args)-1]; got != BuildSRTURL(stream) {
		t.Fatalf("caller output = %q, want SRT URL %q", got, BuildSRTURL(stream))
	}
}

func TestBuildListenerPipeArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		codec     types.Codec
		wantCodec string
		wantFmt   string
		wantPAT   bool
	}{
		{
			name:      "mp3 writes raw mp3 pipe",
			codec:     types.CodecMP3,
			wantCodec: "libmp3lame",
			wantFmt:   "mp3",
			wantPAT:   false,
		},
		{
			name:      "opus writes mpegts pipe with pat cadence",
			codec:     types.CodecOpus,
			wantCodec: "libopus",
			wantFmt:   "mpegts",
			wantPAT:   true,
		},
		{
			name:      "pcm writes mpegts pipe with pat cadence",
			codec:     types.CodecPCM,
			wantCodec: "s302m",
			wantFmt:   "mpegts",
			wantPAT:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args := BuildFFmpegArgs(&types.Stream{
				Mode:  types.StreamModeListener,
				Port:  9000,
				Codec: tt.codec,
			})
			if !slices.Contains(args, tt.wantCodec) {
				t.Fatalf("args %v do not contain codec %q", args, tt.wantCodec)
			}
			if got := args[len(args)-1]; got != "pipe:1" {
				t.Fatalf("listener output = %q, want pipe:1; args=%v", got, args)
			}
			if !containsFlagValue(args, "-f", tt.wantFmt) {
				t.Fatalf("args %v do not contain -f %s", args, tt.wantFmt)
			}
			if got := containsFlagValue(args, "-pat_period", "0.1"); got != tt.wantPAT {
				t.Fatalf("contains -pat_period 0.1 = %v, want %v; args=%v", got, tt.wantPAT, args)
			}
			if stringsContainPrefix(args, "srt://") {
				t.Fatalf("listener pipe args contain SRT URL: %v", args)
			}
		})
	}
}

func containsFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func stringsContainPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
