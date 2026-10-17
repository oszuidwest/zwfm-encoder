package dante

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

type testFlow struct {
	request []byte
	target  *net.UDPAddr
}

type fakeTransmitter struct {
	network                  packetNetwork
	dns, control, media      *memoryPacketConn
	channels                 [2]channelInfo
	interfaces               []net.Interface
	addresses                []net.Addr
	queries                  chan string
	flows                    chan testFlow
	stops                    chan []byte
	dropChannel              string
	malformedChannel         string
	dropFlow                 bool
	flowStatus               uint16
	flowBody                 []byte
	failListenAt, failDialAt int
	listenCount, dialCount   int
	wg                       sync.WaitGroup
	restore                  func()
}

func newFakeTransmitter() *fakeTransmitter {
	h := &fakeTransmitter{
		interfaces: []net.Interface{{Index: 7, Name: "test0", Flags: net.FlagUp | net.FlagMulticast}},
		addresses:  []net.Addr{&net.IPNet{IP: net.IPv4(192, 0, 2, 10), Mask: net.CIDRMask(24, 32)}},
		queries:    make(chan string, 32), flows: make(chan testFlow, 8), stops: make(chan []byte, 8),
		flowStatus: 1, flowBody: []byte{1, 2, 3, 4, 5, 6},
	}
	h.channels = [2]channelInfo{
		{port: 4444, id: 1, rate: 48000, bits: 16, nchan: 2, fppMax: 32, fppMin: 1},
		{port: 4444, id: 2, rate: 48000, bits: 16, nchan: 2, fppMax: 32, fppMin: 1},
	}
	h.dns = h.network.bind(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 5353}, false)
	h.control = h.network.bind(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 4444}, false)
	h.media = h.network.bind(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 4445}, false)
	h.network.ports["224.0.0.251:5353"] = h.dns
	h.install()
	return h
}

func (h *fakeTransmitter) install() {
	oldList, oldLookup, oldAddresses := listInterfaces, lookupInterface, interfaceAddresses
	oldListen, oldDial, oldQuery := listenDanteUDP, dialDanteUDP, queryDante
	h.restore = func() {
		listInterfaces, lookupInterface, interfaceAddresses = oldList, oldLookup, oldAddresses
		listenDanteUDP, dialDanteUDP, queryDante = oldListen, oldDial, oldQuery
	}
	listInterfaces = func() ([]net.Interface, error) { return slices.Clone(h.interfaces), nil }
	lookupInterface = func(name string) (*net.Interface, error) {
		for i := range h.interfaces {
			if h.interfaces[i].Name == name {
				return &h.interfaces[i], nil
			}
		}
		return nil, errors.New("unknown test interface")
	}
	interfaceAddresses = func(*net.Interface) ([]net.Addr, error) { return h.addresses, nil }
	listenDanteUDP = func(local *net.UDPAddr) (net.PacketConn, error) {
		h.listenCount++
		if h.listenCount == h.failListenAt {
			return nil, errors.New("injected listen failure")
		}
		return h.network.bind(local, true), nil
	}
	dialDanteUDP = func(local, remote *net.UDPAddr) (net.Conn, error) {
		h.dialCount++
		if h.dialCount == h.failDialAt {
			return nil, errors.New("injected dial failure")
		}
		if local == nil {
			local = &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10)}
		}
		conn := h.network.bind(local, true)
		conn.remote = remote
		return conn, nil
	}
	queryDante = func(conn net.PacketConn, _ []net.Interface, query []byte, destination *net.UDPAddr) error {
		_, err := conn.WriteTo(query, destination)
		return err
	}
}

func (h *fakeTransmitter) start(t *testing.T) {
	t.Helper()
	responses := make(map[string][]byte)
	for i, channel := range h.channels {
		name := []string{"Left", "Right"}[i]
		instance := name + "@tx" + serviceSuffix
		txt := []string{
			fmt.Sprintf("id=%d", channel.id), fmt.Sprintf("rate=%d", channel.rate), fmt.Sprintf("enc=%d", channel.bits),
			fmt.Sprintf("nchan=%d", channel.nchan), fmt.Sprintf("fpp=%d,%d", channel.fppMax, channel.fppMin),
		}
		responses[instance] = buildTestDNSResponse(t, []testChannel{{instance: instance, port: channel.port, txt: txt}}, nil)
	}
	h.wg.Go(func() { h.serveDNS(t, responses) })
	h.wg.Go(func() { h.serveControl(t) })
}

func (h *fakeTransmitter) serveDNS(t *testing.T, responses map[string][]byte) {
	buffer := make([]byte, 4096)
	for {
		n, peer, err := h.dns.ReadFrom(buffer)
		if err != nil {
			return
		}
		var message dnsmessage.Message
		if err := message.Unpack(buffer[:n]); err != nil || len(message.Questions) != 2 {
			t.Errorf("invalid discovery request: %+v, %v", message, err)
			continue
		}
		instance := message.Questions[0].Name.String()
		if message.Header.Response || message.Questions[0].Type != dnsmessage.TypeSRV || message.Questions[1].Type != dnsmessage.TypeTXT ||
			message.Questions[0].Class != dnsmessage.ClassINET || message.Questions[1].Class != dnsmessage.ClassINET ||
			message.Questions[1].Name.String() != instance {
			t.Errorf("unexpected discovery questions: %+v", message)
			continue
		}
		h.queries <- instance
		if strings.HasPrefix(instance, h.dropChannel+"@") {
			continue
		}
		response := responses[instance]
		if strings.HasPrefix(instance, h.malformedChannel+"@") {
			response = []byte{0, 1}
		}
		if _, err := h.dns.WriteTo(response, peer); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	}
}

func (h *fakeTransmitter) serveControl(t *testing.T) {
	buffer := make([]byte, 4096)
	for {
		n, peer, err := h.control.ReadFrom(buffer)
		if err != nil {
			return
		}
		request := slices.Clone(buffer[:n])
		if n < controlHeaderSize || int(binary.BigEndian.Uint16(request[2:4])) != n {
			t.Errorf("invalid control request: %x", request)
			continue
		}
		sequence, opcode := binary.BigEndian.Uint16(request[4:6]), binary.BigEndian.Uint16(request[6:8])
		status, body := uint16(1), []byte(nil)
		switch opcode {
		case requestFlowOpcode:
			target, err := flowMediaTarget(request)
			if err != nil {
				t.Error(err)
				continue
			}
			h.flows <- testFlow{request: request, target: target}
			if h.dropFlow {
				continue
			}
			status, body = h.flowStatus, h.flowBody
		case stopFlowOpcode:
			h.stops <- request
		default:
			t.Errorf("unexpected control opcode %#x", opcode)
			continue
		}
		if _, err := h.control.WriteTo(controlResponse(sequence, opcode, status, body), peer); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	}
}

func flowMediaTarget(request []byte) (*net.UDPAddr, error) {
	if len(request) < controlHeaderSize+16 {
		return nil, errors.New("short flow request")
	}
	offset := int(binary.BigEndian.Uint16(request[controlHeaderSize+14:]))
	if offset+8 > len(request) {
		return nil, errors.New("invalid media address offset")
	}
	return &net.UDPAddr{IP: net.IP(slices.Clone(request[offset+4 : offset+8])), Port: int(binary.BigEndian.Uint16(request[offset+2:]))}, nil
}

func (h *fakeTransmitter) close(t *testing.T) {
	t.Helper()
	defer h.restore()
	for _, client := range h.network.clients {
		select {
		case <-client.closed:
		default:
			t.Errorf("receiver leaked socket %s -> %v", client.LocalAddr(), client.RemoteAddr())
			_ = client.Close()
		}
	}
	_ = h.dns.Close()
	_ = h.control.Close()
	_ = h.media.Close()
	h.wg.Wait()
	if len(h.network.ports) != 0 {
		t.Errorf("packet network still has %d open endpoints", len(h.network.ports))
	}
}
