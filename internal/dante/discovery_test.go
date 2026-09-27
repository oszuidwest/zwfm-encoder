package dante

import (
	"net"
	"reflect"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestParseTXT(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		items     []string
		want      channelInfo
		wantError bool
	}{
		{
			name: "observed record",
			items: []string{
				"txtvers=2", "id=0x10", "rate=48000", "en=24", "ignored",
				"enc=32", "fpp=32,16", "nchan=2",
			},
			want: channelInfo{
				id: 16, rate: 48000, bits: 32, nchan: 2, fppMax: 32, fppMin: 16,
			},
		},
		{
			name:  "decimal ID and en fallback",
			items: []string{"id=9", "rate=48000", "en=16", "nchan=4", "fpp=64,8"},
			want: channelInfo{
				id: 9, rate: 48000, bits: 16, nchan: 4, fppMax: 64, fppMin: 8,
			},
		},
		{name: "missing ID", items: []string{"rate=48000", "enc=24", "nchan=2", "fpp=32,32"}, wantError: true},
		{name: "ID overflow", items: []string{"id=65536", "rate=48000", "enc=24", "nchan=2", "fpp=32,32"}, wantError: true},
		{name: "malformed rate", items: []string{"id=1", "rate=x", "enc=24", "nchan=2", "fpp=32,32"}, wantError: true},
		{name: "preferred enc malformed", items: []string{"id=1", "rate=48000", "en=24", "enc=x", "nchan=2", "fpp=32,32"}, wantError: true},
		{name: "malformed fpp", items: []string{"id=1", "rate=48000", "enc=24", "nchan=2", "fpp=32"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTXT(test.items)
			if test.wantError {
				if err == nil {
					t.Fatalf("parseTXT(%q) unexpectedly succeeded: %#v", test.items, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTXT(%q): %v", test.items, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseTXT(%q) = %#v, want %#v", test.items, got, test.want)
			}
		})
	}
}

func TestParseDNSResponseSelectsRequestedInstance(t *testing.T) {
	t.Parallel()
	requested := "Left@tx" + serviceSuffix
	other := "Other@tx" + serviceSuffix
	packet := buildMixedDNSResponse(t, requested, other)

	port, txt, err := parseDNSResponse(packet, requested)
	if err != nil {
		t.Fatalf("parseDNSResponse: %v", err)
	}
	if port != 4455 {
		t.Fatalf("port = %d, want 4455", port)
	}
	if txt == nil {
		t.Fatal("TXT result is nil")
	}
	if txt.id != 7 || txt.bits != 24 || txt.rate != 48000 {
		t.Fatalf("TXT result = %#v", txt)
	}
}

func TestMergeDiscoveryResponseAcceptsOnlyRequiredSource(t *testing.T) {
	t.Parallel()
	requested := "Right@tx" + serviceSuffix
	packet := buildMixedDNSResponse(t, requested, "Other@tx"+serviceSuffix)
	requiredSource := net.IPv4(192, 0, 2, 10)
	foreignSource := net.IPv4(192, 0, 2, 11)
	var info channelInfo

	complete, err := mergeDiscoveryResponse(&info, packet, foreignSource, requested, requiredSource)
	if err != nil {
		t.Fatalf("foreign response: %v", err)
	}
	if complete || info.address != nil {
		t.Fatalf("foreign response was accepted: %#v", info)
	}

	complete, err = mergeDiscoveryResponse(&info, packet, requiredSource, requested, requiredSource)
	if err != nil {
		t.Fatalf("required response: %v", err)
	}
	if !complete {
		t.Fatal("required response did not complete discovery")
	}
	if !info.address.Equal(requiredSource) {
		t.Fatalf("source = %v, want %v", info.address, requiredSource)
	}
}

func TestMergeDiscoveryResponseIgnoresMalformedTXT(t *testing.T) {
	t.Parallel()
	requested := "Left@tx" + serviceSuffix
	source := net.IPv4(192, 0, 2, 10)
	malformed := buildTestDNSResponse(t, []testChannel{
		{requested, 4455, []string{"id=not-a-number", "rate=48000", "enc=24", "nchan=2", "fpp=32,32"}},
	}, nil)
	valid := buildTestDNSResponse(t, []testChannel{
		{requested, 4455, []string{"id=7", "rate=48000", "enc=24", "nchan=2", "fpp=32,32"}},
	}, nil)
	var info channelInfo

	complete, err := mergeDiscoveryResponse(&info, malformed, source, requested, nil)
	if err == nil {
		t.Fatal("malformed TXT did not report a parse error")
	}
	if complete || info.address != nil {
		t.Fatalf("malformed TXT changed discovery state: %#v", info)
	}

	complete, err = mergeDiscoveryResponse(&info, valid, source, requested, nil)
	if err != nil {
		t.Fatalf("valid response after malformed TXT: %v", err)
	}
	if !complete || info.id != 7 {
		t.Fatalf("valid response was not accepted: %#v", info)
	}
}

type testChannel struct {
	instance string
	port     uint16
	txt      []string
}

// buildTestDNSResponse encodes SRV and TXT records for each channel into the
// answer and additional sections of an mDNS response.
func buildTestDNSResponse(t *testing.T, answers, additionals []testChannel) []byte {
	t.Helper()
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	builder.EnableCompression()
	add := func(channels []testChannel) {
		for _, channel := range channels {
			header := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(channel.instance), Class: dnsmessage.ClassINET, TTL: 120}
			srv := dnsmessage.SRVResource{Port: channel.port, Target: dnsmessage.MustNewName("tx.local.")}
			if err := builder.SRVResource(header, srv); err != nil {
				t.Fatal(err)
			}
			if err := builder.TXTResource(header, dnsmessage.TXTResource{TXT: channel.txt}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := builder.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	add(answers)
	if err := builder.StartAdditionals(); err != nil {
		t.Fatal(err)
	}
	add(additionals)
	packet, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

// buildMixedDNSResponse answers for other and puts requested in the additional section.
func buildMixedDNSResponse(t *testing.T, requested, other string) []byte {
	t.Helper()
	return buildTestDNSResponse(t,
		[]testChannel{{other, 9999, []string{"id=99", "rate=44100", "enc=16", "nchan=1", "fpp=1,1"}}},
		[]testChannel{{requested, 4455, []string{"id=7", "rate=48000", "enc=24", "nchan=2", "fpp=32,32"}}},
	)
}
