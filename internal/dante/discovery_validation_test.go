package dante

import (
	"cmp"
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDiscoveryRequiresLocalMDNSSource(t *testing.T) {
	t.Parallel()
	_, network, err := net.ParseCIDR("192.0.2.10/24")
	if err != nil {
		t.Fatal(err)
	}
	_, other, err := net.ParseCIDR("198.51.100.10/24")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, ip string
		port     int
		ifIndex  int
		want     bool
	}{
		{name: "local mDNS", ip: "192.0.2.11", port: 5353, ifIndex: 1, want: true},
		{name: "wrong port", ip: "192.0.2.11", port: 4455, ifIndex: 1},
		{name: "off link", ip: "203.0.113.11", port: 5353, ifIndex: 1},
		{name: "ipv6", ip: "2001:db8::1", port: 5353, ifIndex: 1},
		{name: "other selected interface subnet", ip: "198.51.100.11", port: 5353, ifIndex: 1},
		{name: "other selected interface local", ip: "198.51.100.11", port: 5353, ifIndex: 2, want: true},
		{name: "missing receive interface", ip: "192.0.2.11", port: 5353},
		{name: "unselected receive interface", ip: "192.0.2.11", port: 5353, ifIndex: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := validDiscoverySource(&net.UDPAddr{IP: net.ParseIP(tt.ip), Port: tt.port}, tt.ifIndex, map[int][]*net.IPNet{1: {network}, 2: {other}}); got != tt.want {
				t.Fatalf("validDiscoverySource = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestDNSResponseRejectsInvalidProtocolFields(t *testing.T) {
	t.Parallel()
	requested := "Left@tx" + serviceSuffix
	question := func(name string, questionType dnsmessage.Type, class dnsmessage.Class) *dnsmessage.Question {
		return &dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: questionType, Class: class}
	}
	// Rows describe a valid response unless they set query or override a field.
	for _, tt := range []struct {
		name     string
		query    bool
		header   dnsmessage.Header
		question *dnsmessage.Question
		class    dnsmessage.Class
		want     bool
		ignored  bool // Skipped like an unrelated record, without a parse error.
	}{
		{name: "valid", want: true},
		{name: "cache flush", class: dnsmessage.ClassINET | 0x8000, want: true},
		{name: "matching question", question: question(requested, dnsmessage.TypeSRV, dnsmessage.ClassINET), want: true},
		{name: "query", query: true},
		{name: "opcode", header: dnsmessage.Header{OpCode: 1}},
		{name: "unrelated id", header: dnsmessage.Header{ID: 123}},
		{name: "rcode", header: dnsmessage.Header{RCode: dnsmessage.RCodeServerFailure}},
		{name: "truncated", header: dnsmessage.Header{Truncated: true}},
		{name: "wrong resource class", class: dnsmessage.ClassCHAOS, ignored: true},
		{name: "unrelated question", question: question("Other@tx"+serviceSuffix, dnsmessage.TypeSRV, dnsmessage.ClassINET)},
		{name: "wrong question class", question: question(requested, dnsmessage.TypeSRV, dnsmessage.ClassCHAOS)},
		{name: "unrequested question type", question: question(requested, dnsmessage.TypeA, dnsmessage.ClassINET)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			header := tt.header
			header.Response = !tt.query
			builder := dnsmessage.NewBuilder(nil, header)
			if tt.question != nil {
				if err := builder.StartQuestions(); err != nil {
					t.Fatal(err)
				}
				if err := builder.Question(*tt.question); err != nil {
					t.Fatal(err)
				}
			}
			if err := builder.StartAnswers(); err != nil {
				t.Fatal(err)
			}
			resource := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(requested), Class: cmp.Or(tt.class, dnsmessage.ClassINET), TTL: 120}
			if err := builder.SRVResource(resource, dnsmessage.SRVResource{Port: 4455, Target: dnsmessage.MustNewName("tx.local.")}); err != nil {
				t.Fatal(err)
			}
			if err := builder.TXTResource(resource, dnsmessage.TXTResource{TXT: []string{"id=1", "rate=48000", "enc=24", "nchan=2", "fpp=32,32"}}); err != nil {
				t.Fatal(err)
			}
			packet, err := builder.Finish()
			if err != nil {
				t.Fatal(err)
			}
			var info channelInfo
			complete, err := mergeDiscoveryResponse(&info, packet, net.IPv4(192, 0, 2, 11), requested, nil)
			if complete != tt.want {
				t.Fatalf("complete = %t, error %v, want %t", complete, err, tt.want)
			}
			if !tt.want && info.address != nil {
				t.Fatalf("rejected packet contributed discovery state: %+v", info)
			}
			if tt.ignored && err != nil {
				t.Fatalf("ignored record reported error %v", err)
			}
		})
	}
}
