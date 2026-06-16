package streaming

import (
	"net/url"
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
