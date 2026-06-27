package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// unmarshalCase describes one JSON enum decode case.
type unmarshalCase[T comparable] struct {
	name    string
	input   string
	want    T
	wantErr string
}

// runUnmarshalCases checks valid and invalid JSON enum decodes.
func runUnmarshalCases[T comparable](t *testing.T, tests []unmarshalCase[T]) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got T
			err := json.Unmarshal([]byte(tt.input), &got)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("UnmarshalJSON() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalJSON() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("UnmarshalJSON() = %v, want %v", got, tt.want)
			}
		})
	}
}

// streamValidateCase describes one stream validation fixture.
type streamValidateCase struct {
	name    string
	stream  *Stream
	wantErr string
}

// streamCase builds a stream validation fixture with an optional expected error.
func streamCase(name string, stream *Stream, wantErr ...string) streamValidateCase {
	tc := streamValidateCase{name: name, stream: stream}
	if len(wantErr) > 0 {
		tc.wantErr = wantErr[0]
	}
	return tc
}

func TestCodecUnmarshalJSON(t *testing.T) {
	runUnmarshalCases(t, []unmarshalCase[Codec]{
		{name: "empty rejected", input: `""`, wantErr: "codec: must be pcm, mp3, or opus"},
		{name: "pcm", input: `"pcm"`, want: CodecPCM},
		{name: "mp3", input: `"mp3"`, want: CodecMP3},
		{name: "opus", input: `"opus"`, want: CodecOpus},
		{name: "legacy wav rejected", input: `"wav"`, wantErr: "codec: must be pcm, mp3, or opus"},
		{name: "legacy ogg rejected", input: `"ogg"`, wantErr: "codec: must be pcm, mp3, or opus"},
	})
}
func TestRecordingModeUnmarshalJSON(t *testing.T) {
	runUnmarshalCases(t, []unmarshalCase[RecordingMode]{
		{name: "empty rejected", input: `""`, wantErr: "recording_mode: must be hourly or ondemand"},
		{name: "hourly", input: `"hourly"`, want: RecordingHourly},
		{name: "ondemand", input: `"ondemand"`, want: RecordingOnDemand},
		{name: "invalid rejected", input: `"manual"`, wantErr: "recording_mode: must be hourly or ondemand"},
	})
}
func TestStorageModeUnmarshalJSON(t *testing.T) {
	runUnmarshalCases(t, []unmarshalCase[StorageMode]{
		{name: "empty rejected", input: `""`, wantErr: "storage_mode: must be local, s3, or both"},
		{name: "local", input: `"local"`, want: StorageLocal},
		{name: "s3", input: `"s3"`, want: StorageS3},
		{name: "both", input: `"both"`, want: StorageBoth},
		{name: "invalid rejected", input: `"remote"`, wantErr: "storage_mode: must be local, s3, or both"},
	})
}
func TestValidateBitrate(t *testing.T) {
	tests := []struct {
		name    string
		codec   Codec
		bitrate int
		wantErr string
	}{
		{name: "opus default", codec: CodecOpus, bitrate: 0},
		{name: "opus min", codec: CodecOpus, bitrate: 64},
		{name: "opus max", codec: CodecOpus, bitrate: 256},
		{name: "opus below min", codec: CodecOpus, bitrate: 63, wantErr: "bitrate: must be between 64 and 256 for Opus"},
		{name: "opus above max", codec: CodecOpus, bitrate: 257, wantErr: "bitrate: must be between 64 and 256 for Opus"},
		{name: "pcm default", codec: CodecPCM, bitrate: 0},
		{name: "pcm rejects non-zero", codec: CodecPCM, bitrate: 128, wantErr: "bitrate: not supported for PCM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBitrate(tt.codec, tt.bitrate)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateBitrate(%q, %d) error = %v, want containing %q", tt.codec, tt.bitrate, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateBitrate(%q, %d) error = %v", tt.codec, tt.bitrate, err)
			}
		})
	}
}
func TestBuildCodecArgs(t *testing.T) {
	tests := []struct {
		name    string
		codec   Codec
		bitrate int
		want    []string
	}{
		{name: "mp3 default", codec: CodecMP3, bitrate: 0, want: []string{"libmp3lame", "-b:a", "320k"}},
		{name: "mp3 custom bitrate", codec: CodecMP3, bitrate: 192, want: []string{"libmp3lame", "-b:a", "192k"}},
		{name: "opus default includes frame duration", codec: CodecOpus, bitrate: 0, want: []string{"libopus", "-b:a", "128k", "-frame_duration", "10"}},
		{name: "opus custom bitrate includes frame duration", codec: CodecOpus, bitrate: 96, want: []string{"libopus", "-b:a", "96k", "-frame_duration", "10"}},
		{name: "pcm", codec: CodecPCM, bitrate: 0, want: []string{"s302m", "-strict", "-2"}},
		{name: "unknown falls back to pcm", codec: Codec("aac"), bitrate: 0, want: []string{"s302m", "-strict", "-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCodecArgs(tt.codec, tt.bitrate)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildCodecArgs(%q, %d) = %#v, want %#v", tt.codec, tt.bitrate, got, tt.want)
			}
		})
	}
}
func TestCodecFormat(t *testing.T) {
	tests := []struct {
		codec Codec
		want  string
	}{
		{codec: CodecMP3, want: "mp3"},
		{codec: CodecOpus, want: "mpegts"},
		{codec: CodecPCM, want: "mpegts"},
		{codec: Codec("aac"), want: "mpegts"},
	}
	for _, tt := range tests {
		t.Run(string(tt.codec), func(t *testing.T) {
			if got := tt.codec.Format(); got != tt.want {
				t.Fatalf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}
func TestCodecDefaultBitrate(t *testing.T) {
	tests := []struct {
		codec Codec
		want  int
	}{
		{codec: CodecMP3, want: 320},
		{codec: CodecOpus, want: 128},
		{codec: CodecPCM, want: 0},
		{codec: Codec("aac"), want: 0},
	}
	for _, tt := range tests {
		t.Run(string(tt.codec), func(t *testing.T) {
			if got := tt.codec.DefaultBitrate(); got != tt.want {
				t.Fatalf("DefaultBitrate() = %d, want %d", got, tt.want)
			}
		})
	}
}
func TestStreamModeOrDefault(t *testing.T) {
	tests := []struct {
		name string
		mode StreamMode
		want StreamMode
	}{
		{name: "legacy empty defaults to caller", want: StreamModeCaller},
		{name: "caller", mode: StreamModeCaller, want: StreamModeCaller},
		{name: "listener", mode: StreamModeListener, want: StreamModeListener},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.OrDefault(); got != tt.want {
				t.Fatalf("OrDefault() = %q, want %q", got, tt.want)
			}
		})
	}
}
func TestStreamValidateModeAware(t *testing.T) {
	tests := []streamValidateCase{
		streamCase("legacy caller remains valid", &Stream{Host: "stream.example.com", Port: 9000, Codec: CodecMP3}),
		streamCase("caller requires host", &Stream{Mode: StreamModeCaller, Port: 9000, Codec: CodecMP3}, "host: is required"),
		streamCase("listener allows empty host", &Stream{Mode: StreamModeListener, Port: 9000, Codec: CodecMP3}),
		streamCase("listener rejects stream id", &Stream{Mode: StreamModeListener, Port: 9000, Codec: CodecMP3, StreamID: "studio"}, "stream_id: not supported for listener mode"),
		streamCase("invalid mode", &Stream{Mode: StreamMode("pull"), Host: "stream.example.com", Port: 9000, Codec: CodecMP3}, "mode: must be caller or listener"),
		streamCase("empty password allowed", &Stream{Host: "stream.example.com", Port: 9000, Codec: CodecMP3}),
		streamCase("password min length accepted", &Stream{Host: "stream.example.com", Port: 9000, Codec: CodecMP3, Password: "1234567890"}),
		streamCase("short password rejected", &Stream{Host: "stream.example.com", Port: 9000, Codec: CodecMP3, Password: "short"}, "password: must be empty or between 10 and 64 characters"),
		streamCase("long password rejected", &Stream{Host: "stream.example.com", Port: 9000, Codec: CodecMP3, Password: strings.Repeat("x", 65)}, "password: must be empty or between 10 and 64 characters"),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.stream.Validate()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
func TestEventSubscriptionsToZabbixEventSubscriptionsOmitsAudioDump(t *testing.T) {
	got := (EventSubscriptions{
		SilenceStart:          true,
		SilenceEnd:            false,
		AudioDump:             true,
		ChannelImbalanceStart: true,
		ChannelImbalanceEnd:   true,
	}).ToZabbixEventSubscriptions()
	want := ZabbixEventSubscriptions{
		SilenceStart:          true,
		SilenceEnd:            false,
		ChannelImbalanceStart: true,
		ChannelImbalanceEnd:   true,
	}
	if got != want {
		t.Fatalf("ToZabbixEventSubscriptions() = %+v, want %+v", got, want)
	}
}
func TestZabbixEventSubscriptionsRoundTrip(t *testing.T) {
	start := ZabbixEventSubscriptions{
		SilenceStart:          true,
		SilenceEnd:            true,
		ChannelImbalanceStart: true,
		ChannelImbalanceEnd:   true,
	}
	got := start.ToEventSubscriptions().ToZabbixEventSubscriptions()
	if got != start {
		t.Fatalf("round-trip = %+v, want %+v", got, start)
	}
}
