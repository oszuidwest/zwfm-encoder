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
		{name: "empty rejected", input: `""`, wantErr: "codec: must be pcm, mp3, or opus"},
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

func TestRecordingModeUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    RecordingMode
		wantErr string
	}{
		{name: "empty rejected", input: `""`, wantErr: "recording_mode: must be hourly or ondemand"},
		{name: "hourly", input: `"hourly"`, want: RecordingHourly},
		{name: "ondemand", input: `"ondemand"`, want: RecordingOnDemand},
		{name: "invalid rejected", input: `"manual"`, wantErr: "recording_mode: must be hourly or ondemand"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got RecordingMode
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

func TestStorageModeUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    StorageMode
		wantErr string
	}{
		{name: "empty rejected", input: `""`, wantErr: "storage_mode: must be local, s3, or both"},
		{name: "local", input: `"local"`, want: StorageLocal},
		{name: "s3", input: `"s3"`, want: StorageS3},
		{name: "both", input: `"both"`, want: StorageBoth},
		{name: "invalid rejected", input: `"remote"`, wantErr: "storage_mode: must be local, s3, or both"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got StorageMode
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

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

func TestStreamModeOrDefault(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			if got := tt.mode.OrDefault(); got != tt.want {
				t.Fatalf("OrDefault() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStreamValidateModeAware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		stream  Stream
		wantErr string
	}{
		{
			name: "legacy caller remains valid",
			stream: Stream{
				Host:  "stream.example.com",
				Port:  9000,
				Codec: CodecMP3,
			},
		},
		{
			name: "caller requires host",
			stream: Stream{
				Mode:  StreamModeCaller,
				Port:  9000,
				Codec: CodecMP3,
			},
			wantErr: "host: is required",
		},
		{
			name: "listener allows empty host",
			stream: Stream{
				Mode:  StreamModeListener,
				Port:  9000,
				Codec: CodecMP3,
			},
		},
		{
			name: "listener rejects stream id",
			stream: Stream{
				Mode:     StreamModeListener,
				Port:     9000,
				Codec:    CodecMP3,
				StreamID: "studio",
			},
			wantErr: "stream_id: not supported for listener mode",
		},
		{
			name: "invalid mode",
			stream: Stream{
				Mode:  StreamMode("pull"),
				Host:  "stream.example.com",
				Port:  9000,
				Codec: CodecMP3,
			},
			wantErr: "mode: must be caller or listener",
		},
		{
			name: "empty password allowed",
			stream: Stream{
				Host:  "stream.example.com",
				Port:  9000,
				Codec: CodecMP3,
			},
		},
		{
			name: "password min length accepted",
			stream: Stream{
				Host:     "stream.example.com",
				Port:     9000,
				Codec:    CodecMP3,
				Password: "1234567890",
			},
		},
		{
			name: "short password rejected",
			stream: Stream{
				Host:     "stream.example.com",
				Port:     9000,
				Codec:    CodecMP3,
				Password: "short",
			},
			wantErr: "password: must be empty or between 10 and 64 characters",
		},
		{
			name: "long password rejected",
			stream: Stream{
				Host:     "stream.example.com",
				Port:     9000,
				Codec:    CodecMP3,
				Password: strings.Repeat("x", 65),
			},
			wantErr: "password: must be empty or between 10 and 64 characters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

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
