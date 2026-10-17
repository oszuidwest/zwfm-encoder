package dante

import (
	"context"
	"net/url"
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

func TestInputNameValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, transmitter, left, right, wantError string
	}{
		{name: "31 character names", transmitter: strings.Repeat("a", 31), left: strings.Repeat("L", 31), right: strings.Repeat("R", 31)},
		{name: "single character device", transmitter: "1", left: "L", right: "R"},
		{name: "encoded punctuation", transmitter: "Studio-1", left: "A/B ?#", right: "C/D ?#"},
		{name: "long device", transmitter: strings.Repeat("a", 32), left: "L", right: "R", wantError: "transmitter name must"},
		{name: "leading hyphen", transmitter: "-tx", left: "L", right: "R", wantError: "transmitter name must"},
		{name: "trailing hyphen", transmitter: "tx-", left: "L", right: "R", wantError: "transmitter name must"},
		{name: "unicode device", transmitter: "日本", left: "L", right: "R", wantError: "transmitter name must"},
		{name: "port", transmitter: "tx:4455", left: "L", right: "R", wantError: "transmitter name must"},
		{name: "domain", transmitter: "tx.local", left: "L", right: "R", wantError: "transmitter name must"},
		{name: "underscore", transmitter: "studio_tx", left: "L", right: "R", wantError: "transmitter name must"},
		{name: "long left", transmitter: "tx", left: strings.Repeat("L", 32), right: "R", wantError: "left channel:"},
		{name: "long right", transmitter: "tx", left: "L", right: strings.Repeat("R", 32), wantError: "right channel:"},
		{name: "unicode channel", transmitter: "tx", left: "L", right: "日本", wantError: "right channel:"},
		{name: "equals", transmitter: "tx", left: "L=1", right: "R", wantError: "left channel:"},
		{name: "dot", transmitter: "tx", left: "L", right: "R.1", wantError: "right channel:"},
		{name: "at", transmitter: "tx", left: "L@1", right: "R", wantError: "left channel:"},
		{name: "control character", transmitter: "tx", left: "L\n", right: "R", wantError: "left channel:"},
		{name: "nul", transmitter: "tx", left: "L", right: "R\x00", wantError: "right channel:"},
		{name: "delete", transmitter: "tx", left: "L\x7f", right: "R", wantError: "left channel:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := "dante://" + tt.transmitter + "/" + url.PathEscape(tt.left) + "/" + url.PathEscape(tt.right)
			got, err := parseInput(input)
			if tt.wantError == "" {
				if err != nil || got.transmitter != tt.transmitter || got.left != tt.left || got.right != tt.right {
					t.Fatalf("parseInput(%q) = %+v, %v", input, got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("parseInput(%q) error = %v, want %s", input, err, tt.wantError)
			}
			// An invalid interface distinguishes validation from later socket setup.
			_, err = Open(context.Background(), input+"?interface=does-not-exist")
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Open(%q) error = %v, want name error before network setup", input, err)
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
