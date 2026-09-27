package dante

import (
	"errors"
	"testing"
	"time"
)

func TestParseInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		want         Config
		wantErr      bool
		wantNotDante bool
	}{
		{
			name:  "canonical URI",
			input: "dante://studio-tx/Program%20L/Program%20R?interface=eth0&receiver=Encoder&reorder=6ms",
			want: Config{
				Transmitter: "studio-tx",
				Channels:    [2]string{"Program L", "Program R"},
				Interface:   "eth0",
				Receiver:    "Encoder",
				ReorderWait: 6 * time.Millisecond,
			},
		},
		{
			name:  "query names",
			input: "dante:?tx=Main%20Desk&left=Main%20L&right=Main%20R",
			want: Config{
				Transmitter: "Main Desk",
				Channels:    [2]string{"Main L", "Main R"},
				ReorderWait: defaultReorderWait,
			},
		},
		{
			name:         "different scheme",
			input:        "alsa://hw:0",
			wantErr:      true,
			wantNotDante: true,
		},
		{
			name:    "missing right channel",
			input:   "dante://studio-tx/Left",
			wantErr: true,
		},
		{
			name:    "invalid reorder duration",
			input:   "dante://studio-tx/Left/Right?reorder=forever",
			wantErr: true,
		},
		{
			name:    "excessive reorder duration",
			input:   "dante://studio-tx/Left/Right?reorder=2s",
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseInput(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseInput(%q) error = nil", test.input)
				}
				if test.wantNotDante && !errors.Is(err, ErrNotDanteInput) {
					t.Fatalf("ParseInput(%q) error = %v, want ErrNotDanteInput", test.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInput(%q) error = %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("ParseInput(%q) = %#v, want %#v", test.input, got, test.want)
			}
		})
	}
}

func TestIsInput(t *testing.T) {
	t.Parallel()

	if !IsInput("DANTE://tx/left/right") {
		t.Fatal("IsInput() rejected a Dante URI")
	}
	if IsInput("default:CARD=audio") {
		t.Fatal("IsInput() accepted a platform audio device")
	}
}
