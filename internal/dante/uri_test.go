package dante

import (
	"strings"
	"testing"
)

func TestIsInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "lowercase", input: "dante://tx/left/right", want: true},
		{name: "uppercase", input: "DANTE://tx/left/right", want: true},
		{name: "other scheme", input: "https://tx/left/right", want: false},
		{name: "not a URL", input: "://", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := IsInput(test.input); got != test.want {
				t.Fatalf("IsInput(%q) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}

func TestParseInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		want      inputConfig
		wantError bool
	}{
		{
			name:  "ordinary",
			input: "dante://studio-tx/Program%20L/Program%20R",
			want: inputConfig{
				transmitter: "studio-tx",
				left:        "Program L",
				right:       "Program R",
			},
		},
		{
			name:  "encoded slash and interface",
			input: "DANTE://tx/A%2FB/C%2FD?interface=en0",
			want: inputConfig{
				transmitter: "tx",
				left:        "A/B",
				right:       "C/D",
				interfaceID: "en0",
			},
		},
		{name: "missing transmitter", input: "dante:///left/right", wantError: true},
		{name: "one channel", input: "dante://tx/left", wantError: true},
		{name: "three channels", input: "dante://tx/a/b/c", wantError: true},
		{name: "empty channel", input: "dante://tx//right", wantError: true},
		{name: "unknown query", input: "dante://tx/a/b?other=x", wantError: true},
		{name: "duplicate interface", input: "dante://tx/a/b?interface=x&interface=y", wantError: true},
		{name: "empty interface", input: "dante://tx/a/b?interface=", wantError: true},
		{name: "fragment", input: "dante://tx/a/b#fragment", wantError: true},
		{name: "wrong scheme", input: "udp://tx/a/b", wantError: true},
		{name: "bad escape", input: "dante://tx/%zz/b", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseInput(test.input)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseInput(%q) unexpectedly succeeded: %#v", test.input, got)
				}
				if !strings.Contains(err.Error(), inputFormat) {
					t.Fatalf("error %q does not show input format", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInput(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("parseInput(%q) = %#v, want %#v", test.input, got, test.want)
			}
		})
	}
}
