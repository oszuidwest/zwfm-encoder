package dante

import (
	"net"
	"testing"

	"github.com/grandcat/zeroconf"
)

func TestParseChannelEntry(t *testing.T) {
	t.Parallel()

	entry := &zeroconf.ServiceEntry{
		Port:     4455,
		Text:     []string{"txtvers=2", "dbcp1=0x1102", "id=7", "rate=48000", "enc=24", "fpp=32,4", "nchan=8"},
		AddrIPv4: []net.IP{net.IPv4(192, 0, 2, 10)},
	}

	got, err := parseChannelEntry(entry)
	if err != nil {
		t.Fatalf("parseChannelEntry() error = %v", err)
	}
	if !got.address.IP.Equal(net.IPv4(192, 0, 2, 10)) || got.address.Port != 4455 {
		t.Fatalf("address = %v, want 192.0.2.10:4455", got.address)
	}
	if got.ID != 7 || got.dbcp1 != 0x1102 {
		t.Fatalf("identifiers = id:%d dbcp1:%#x", got.ID, got.dbcp1)
	}
	if got.sampleRate != 48000 || got.bitsPerSample != 24 || got.channelsPerFlow != 8 {
		t.Fatalf("format = rate:%d bits:%d channels:%d", got.sampleRate, got.bitsPerSample, got.channelsPerFlow)
	}
	if got.fppMin != 4 || got.fppMax != 32 {
		t.Fatalf("fpp = %d..%d, want 4..32", got.fppMin, got.fppMax)
	}
}

func TestParseChannelEntryRejectsMissingProperty(t *testing.T) {
	t.Parallel()

	entry := &zeroconf.ServiceEntry{
		Port:     4455,
		Text:     []string{"id=1"},
		AddrIPv4: []net.IP{net.IPv4(192, 0, 2, 10)},
	}
	if _, err := parseChannelEntry(entry); err == nil {
		t.Fatal("parseChannelEntry() error = nil")
	}
}

func TestValidateChannelPair(t *testing.T) {
	t.Parallel()

	base := advertisedChannel{
		address:         &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 4455},
		ID:              1,
		channelsPerFlow: 8,
		bitsPerSample:   24,
		sampleRate:      48000,
		dbcp1:           0x1102,
	}
	right := base
	right.ID = 2

	tests := []struct {
		name    string
		change  func(*advertisedChannel)
		wantErr bool
	}{
		{name: "compatible"},
		{name: "different port", change: func(channel *advertisedChannel) { channel.address.Port++ }, wantErr: true},
		{name: "different depth", change: func(channel *advertisedChannel) { channel.bitsPerSample = 16 }, wantErr: true},
		{name: "wrong rate", change: func(channel *advertisedChannel) { channel.sampleRate = 44100 }, wantErr: true},
		{name: "unsupported depth", change: func(channel *advertisedChannel) { channel.bitsPerSample = 20 }, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			leftCopy := base
			rightCopy := right
			leftAddress := *base.address
			rightAddress := *right.address
			leftCopy.address = &leftAddress
			rightCopy.address = &rightAddress
			if test.change != nil {
				test.change(&rightCopy)
				if test.name == "wrong rate" || test.name == "unsupported depth" {
					leftCopy.sampleRate = rightCopy.sampleRate
					leftCopy.bitsPerSample = rightCopy.bitsPerSample
				}
			}
			channels := [2]advertisedChannel{leftCopy, rightCopy}
			err := validateChannelPair(&channels)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateChannelPair() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
