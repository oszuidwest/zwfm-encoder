package dante

import (
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
	for _, tt := range []struct {
		name     string
		header   dnsmessage.Header
		question *dnsmessage.Question
		class    dnsmessage.Class
		want     bool
	}{
		{name: "valid", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassINET, want: true},
		{name: "cache flush", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassINET | 0x8000, want: true},
		{name: "matching question", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassINET, want: true,
			question: &dnsmessage.Question{Name: dnsmessage.MustNewName(requested), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET}},
		{name: "query", class: dnsmessage.ClassINET},
		{name: "opcode", header: dnsmessage.Header{Response: true, OpCode: 1}, class: dnsmessage.ClassINET},
		{name: "unrelated id", header: dnsmessage.Header{Response: true, ID: 123}, class: dnsmessage.ClassINET},
		{name: "rcode", header: dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeServerFailure}, class: dnsmessage.ClassINET},
		{name: "truncated", header: dnsmessage.Header{Response: true, Truncated: true}, class: dnsmessage.ClassINET},
		{name: "wrong resource class", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassCHAOS},
		{name: "unrelated question", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassINET,
			question: &dnsmessage.Question{Name: dnsmessage.MustNewName("Other@tx" + serviceSuffix), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET}},
		{name: "wrong question class", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassINET,
			question: &dnsmessage.Question{Name: dnsmessage.MustNewName(requested), Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassCHAOS}},
		{name: "unrequested question type", header: dnsmessage.Header{Response: true}, class: dnsmessage.ClassINET,
			question: &dnsmessage.Question{Name: dnsmessage.MustNewName(requested), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			builder := dnsmessage.NewBuilder(nil, tt.header)
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
			resource := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(requested), Class: tt.class, TTL: 120}
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
		})
	}
}
