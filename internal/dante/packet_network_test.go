package dante

import (
	"bytes"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

type testDatagram struct {
	data []byte
	from net.Addr
}

type packetNetwork struct {
	mu       sync.Mutex
	ports    map[string]*memoryPacketConn
	clients  []*memoryPacketConn
	nextPort int
}

func (n *packetNetwork) bind(address *net.UDPAddr, client bool) *memoryPacketConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	local := *address
	if local.IP == nil || local.IP.IsUnspecified() {
		local.IP = net.IPv4(192, 0, 2, 10)
	}
	if local.Port == 0 {
		n.nextPort++
		local.Port = 40000 + n.nextPort
	}
	c := &memoryPacketConn{network: n, local: &local, packets: make(chan testDatagram, 128), closed: make(chan struct{}), deadlineChanged: make(chan struct{})}
	if n.ports == nil {
		n.ports = make(map[string]*memoryPacketConn)
	}
	n.ports[local.String()] = c
	if client {
		n.clients = append(n.clients, c)
	}
	return c
}

type memoryPacketConn struct {
	network         *packetNetwork
	local, remote   *net.UDPAddr
	packets         chan testDatagram
	closed          chan struct{}
	closeOnce       sync.Once
	mu              sync.Mutex
	deadline        time.Time
	deadlineChanged chan struct{}
}

func (c *memoryPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		deadline, changed := c.deadline, c.deadlineChanged
		c.mu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		packet, retry, err := c.readPacket(changed, timeout)
		if timer != nil {
			timer.Stop()
		}
		if retry {
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		return copy(buffer, packet.data), packet.from, nil
	}
}

func (c *memoryPacketConn) readPacket(changed <-chan struct{}, timeout <-chan time.Time) (testDatagram, bool, error) {
	select {
	case <-c.closed:
		return testDatagram{}, false, net.ErrClosed
	default:
	}
	select {
	case packet := <-c.packets:
		return packet, false, nil
	case <-changed:
		return testDatagram{}, true, nil
	case <-c.closed:
		return testDatagram{}, false, net.ErrClosed
	case <-timeout:
		return testDatagram{}, false, os.ErrDeadlineExceeded
	}
}

func (c *memoryPacketConn) WriteTo(data []byte, address net.Addr) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.network.mu.Lock()
	target := c.network.ports[address.String()]
	c.network.mu.Unlock()
	if target == nil {
		return len(data), nil // UDP may drop a packet with no listener.
	}
	select {
	case target.packets <- testDatagram{data: bytes.Clone(data), from: c.local}:
	case <-target.closed:
	default: // Bounded UDP receive buffers may drop packets too.
	}
	return len(data), nil
}

func (c *memoryPacketConn) Read(buffer []byte) (int, error) {
	n, _, err := c.ReadFrom(buffer)
	return n, err
}

func (c *memoryPacketConn) Write(data []byte) (int, error) {
	if c.remote == nil {
		return 0, errors.New("UDP socket is not connected")
	}
	return c.WriteTo(data, c.remote)
}

func (c *memoryPacketConn) LocalAddr() net.Addr  { return c.local }
func (c *memoryPacketConn) RemoteAddr() net.Addr { return c.remote }

func (c *memoryPacketConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.network.mu.Lock()
		for address, conn := range c.network.ports {
			if conn == c {
				delete(c.network.ports, address)
			}
		}
		c.network.mu.Unlock()
	})
	return nil
}

func (c *memoryPacketConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	close(c.deadlineChanged)
	c.deadlineChanged = make(chan struct{})
	return nil
}

// shortcut: Dante never sets write deadlines, implement them if its transport starts using them.
func (c *memoryPacketConn) SetWriteDeadline(time.Time) error     { return nil }
func (c *memoryPacketConn) SetDeadline(deadline time.Time) error { return c.SetReadDeadline(deadline) }

var (
	_ net.PacketConn = (*memoryPacketConn)(nil)
	_ net.Conn       = (*memoryPacketConn)(nil)
)
