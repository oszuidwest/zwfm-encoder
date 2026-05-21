package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCodecUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Codec
		wantErr string
	}{
		{name: "empty defaults to pcm", input: `""`, want: CodecPCM},
		{name: "pcm", input: `"pcm"`, want: CodecPCM},
		{name: "mp3", input: `"mp3"`, want: CodecMP3},
		{name: "opus", input: `"opus"`, want: CodecOpus},
		{name: "legacy wav rejected", input: `"wav"`, wantErr: "codec: must be pcm, mp3, or opus"},
		{name: "legacy ogg rejected", input: `"ogg"`, wantErr: "codec: must be pcm, mp3, or opus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got Codec
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
				t.Fatalf("UnmarshalJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateBitrate(t *testing.T) {
	t.Parallel()

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
		{name: "unknown rejects zero bitrate", codec: Codec(""), bitrate: 0, wantErr: "codec: must be pcm, mp3, or opus"},
		{name: "unknown rejects non-zero bitrate", codec: Codec("aac"), bitrate: 128, wantErr: "codec: must be pcm, mp3, or opus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateBitrate(tt.codec, tt.bitrate)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ValidateBitrate(%q, %d) error = %v, want containing %q", tt.codec, tt.bitrate, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateBitrate(%q, %d) error = %v", tt.codec, tt.bitrate, err)
			}
		})
	}
}

func TestBuildCodecArgs(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			got := BuildCodecArgs(tt.codec, tt.bitrate)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildCodecArgs(%q, %d) = %#v, want %#v", tt.codec, tt.bitrate, got, tt.want)
			}
		})
	}
}

func TestCodecFormat(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			if got := tt.codec.Format(); got != tt.want {
				t.Fatalf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEventSubscriptionsToZabbixEventSubscriptionsOmitsAudioDump(t *testing.T) {
	t.Parallel()

	got := (EventSubscriptions{
		SilenceStart: true,
		SilenceEnd:   false,
		AudioDump:    true,
	}).ToZabbixEventSubscriptions()

	want := ZabbixEventSubscriptions{
		SilenceStart: true,
		SilenceEnd:   false,
	}
	if got != want {
		t.Fatalf("ToZabbixEventSubscriptions() = %+v, want %+v", got, want)
	}
}

func TestZabbixEventSubscriptionsRoundTrip(t *testing.T) {
	t.Parallel()

	start := ZabbixEventSubscriptions{
		SilenceStart: true,
		SilenceEnd:   true,
	}

	got := start.ToEventSubscriptions().ToZabbixEventSubscriptions()
	if got != start {
		t.Fatalf("round-trip = %+v, want %+v", got, start)
	}
}
