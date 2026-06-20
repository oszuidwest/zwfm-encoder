package srtfanout

import (
	"bytes"
	"errors"
	srt "github.com/datarhei/gosrt"
	"github.com/datarhei/gosrt/packet"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
func TestNewServerDefaults(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Config{
		Port:   9000,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.cfg.BindHost != "0.0.0.0" {
		t.Fatalf("BindHost = %q, want 0.0.0.0", server.cfg.BindHost)
	}
	if server.cfg.Latency != DefaultLatency {
		t.Fatalf("Latency = %s, want %s", server.cfg.Latency, DefaultLatency)
	}
	if server.cfg.MaxClients != DefaultMaxClients {
		t.Fatalf("MaxClients = %d, want %d", server.cfg.MaxClients, DefaultMaxClients)
	}
	if server.cfg.QueueChunks != defaultQueueChunks {
		t.Fatalf("QueueChunks = %d, want %d", server.cfg.QueueChunks, defaultQueueChunks)
	}
}
func TestNewServerHonorsQueueChunks(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Config{
		Port:        9000,
		QueueChunks: 64,
		Logger:      testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if server.cfg.QueueChunks != 64 {
		t.Fatalf("QueueChunks = %d, want 64", server.cfg.QueueChunks)
	}
	sub := server.addQueueOnlySubscriber(t)
	if got := cap(sub.ch); got != 64 {
		t.Fatalf("subscriber queue cap = %d, want 64", got)
	}
}
func TestHandleConnectTreatsEveryStreamIDAsSubscriber(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Config{Port: 9000, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	for _, streamID := range []string{"", "read:studio", "studio", "publish:ignored"} {
		t.Run(streamID, func(t *testing.T) {
			t.Parallel()
			req := &fakeRequest{streamID: streamID}
			if got := server.handleConnect(req); got != srt.SUBSCRIBE {
				t.Fatalf("handleConnect() = %s, want SUBSCRIBE", got)
			}
			if req.passphraseCalls != 0 {
				t.Fatalf("SetPassphrase called %d times, want 0", req.passphraseCalls)
			}
		})
	}
}
func TestHandleConnectRequiresEncryptionWhenPasswordIsSet(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Config{
		Port:     9000,
		Password: "1234567890",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	req := &fakeRequest{encrypted: false}
	if got := server.handleConnect(req); got != srt.REJECT {
		t.Fatalf("unencrypted handleConnect() = %s, want REJECT", got)
	}
	if req.passphraseCalls != 0 {
		t.Fatalf("unencrypted request SetPassphrase calls = %d, want 0", req.passphraseCalls)
	}
	req = &fakeRequest{encrypted: true}
	if got := server.handleConnect(req); got != srt.SUBSCRIBE {
		t.Fatalf("encrypted handleConnect() = %s, want SUBSCRIBE", got)
	}
	if req.passphrase != "1234567890" {
		t.Fatalf("SetPassphrase argument = %q, want configured password", req.passphrase)
	}
}
func TestHandleConnectRejectsPassphraseErrors(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Config{
		Port:     9000,
		Password: "1234567890",
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	req := &fakeRequest{encrypted: true, passphraseErr: errors.New("bad secret")}
	if got := server.handleConnect(req); got != srt.REJECT {
		t.Fatalf("handleConnect() = %s, want REJECT", got)
	}
}
func TestHandleConnectRejectsAtMaxClients(t *testing.T) {
	t.Parallel()
	server, err := NewServer(Config{
		Port:       9000,
		MaxClients: 1,
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server.clientCount.Store(1)
	if got := server.handleConnect(&fakeRequest{}); got != srt.REJECT {
		t.Fatalf("handleConnect() = %s, want REJECT", got)
	}
}
func TestWriteDropsOldestAndKeepsNewest(t *testing.T) {
	t.Parallel()
	server := newQueueTestServer(t)
	sub := server.addQueueOnlySubscriber(t)
	server.Write([]byte("one"))
	server.Write([]byte("two"))
	server.Write([]byte("three"))
	if got := len(sub.ch); got != defaultQueueChunks {
		t.Fatalf("queue len = %d, want %d", got, defaultQueueChunks)
	}
	if got := string(<-sub.ch); got != "two" {
		t.Fatalf("first queued chunk = %q, want two", got)
	}
	if got := string(<-sub.ch); got != "three" {
		t.Fatalf("second queued chunk = %q, want three", got)
	}
	if got := atomic.LoadInt64(&sub.drops); got != 1 {
		t.Fatalf("drops = %d, want 1", got)
	}
	if got := server.DropCount(); got != 1 {
		t.Fatalf("DropCount() = %d, want 1", got)
	}
}
func TestWriteToleratesBurstWithinQueueDepth(t *testing.T) {
	t.Parallel()
	const depth = 32
	server, err := NewServer(Config{Port: 9000, QueueChunks: depth, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	sub := server.addQueueOnlySubscriber(t)
	// Filling the codec-sized queue must not drop.
	for i := 0; i < depth; i++ {
		server.Write([]byte{byte(i)})
	}
	if got := server.DropCount(); got != 0 {
		t.Fatalf("DropCount() after burst of %d into depth %d = %d, want 0", depth, depth, got)
	}
	// One chunk past depth drops exactly one.
	server.Write([]byte{0xff})
	if got := server.DropCount(); got != 1 {
		t.Fatalf("DropCount() after overflow = %d, want 1", got)
	}
	if got := len(sub.ch); got != depth {
		t.Fatalf("queue len = %d, want %d", got, depth)
	}
}
func TestWriteCopiesChunkBeforeEnqueue(t *testing.T) {
	t.Parallel()
	server := newQueueTestServer(t)
	sub := server.addQueueOnlySubscriber(t)
	chunk := []byte("live")
	server.Write(chunk)
	chunk[0] = 'x'
	if got := string(<-sub.ch); got != "live" {
		t.Fatalf("queued chunk = %q, want immutable copy", got)
	}
}
func TestWriteDoesNotBlockOnFullSubscriberQueue(t *testing.T) {
	t.Parallel()
	server := newQueueTestServer(t)
	server.addQueueOnlySubscriber(t)
	server.Write([]byte("one"))
	server.Write([]byte("two"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Write([]byte("three"))
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Write blocked on a full subscriber queue")
	}
}
func TestHandleSubscribeIncrementsDecrementsAndShutdownWaits(t *testing.T) {
	t.Parallel()
	server := newQueueTestServer(t)
	conn := newFakeConn()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleSubscribe(conn)
	}()
	eventually(t, func() bool {
		return server.ClientCount() == 1
	}, "subscriber count to reach 1")
	server.Shutdown()
	if err := server.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleSubscribe did not return after Shutdown")
	}
	if !conn.isClosed() {
		t.Fatal("subscriber connection was not closed")
	}
	if got := server.ClientCount(); got != 0 {
		t.Fatalf("ClientCount() = %d, want 0", got)
	}
}
func TestHandleSubscribeDecrementsAfterClientWriteFailure(t *testing.T) {
	t.Parallel()
	server := newQueueTestServer(t)
	conn := newFakeConn()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleSubscribe(conn)
	}()
	eventually(t, func() bool {
		return server.ClientCount() == 1
	}, "subscriber count to reach 1")
	if err := conn.Close(); err != nil {
		t.Fatalf("fake connection Close() error = %v", err)
	}
	server.Write([]byte("after-disconnect"))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleSubscribe did not return after client write failure")
	}
	if got := server.ClientCount(); got != 0 {
		t.Fatalf("ClientCount() = %d, want 0", got)
	}
}
func TestIntegrationTwoSubscribersReceiveBytes(t *testing.T) {
	port := freeUDPPort(t)
	server, err := NewServer(Config{
		StreamID:   "stream-1",
		BindHost:   "127.0.0.1",
		Port:       port,
		Latency:    50 * time.Millisecond,
		MaxClients: 2,
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		server.Shutdown()
		if err := server.Wait(); err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	}()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn1 := dialSubscriber(t, addr, "read:stream-1")
	defer func() {
		if err := conn1.Close(); err != nil {
			t.Fatalf("conn1 Close() error = %v", err)
		}
	}()
	conn2 := dialSubscriber(t, addr, "other-stream-id")
	defer func() {
		if err := conn2.Close(); err != nil {
			t.Fatalf("conn2 Close() error = %v", err)
		}
	}()
	eventually(t, func() bool {
		return server.ClientCount() == 2
	}, "subscriber count to reach 2")
	server.Write([]byte("hello subscribers"))
	if got := readSRT(t, conn1); got != "hello subscribers" {
		t.Fatalf("conn1 read = %q, want hello subscribers", got)
	}
	if got := readSRT(t, conn2); got != "hello subscribers" {
		t.Fatalf("conn2 read = %q, want hello subscribers", got)
	}
}
func TestIntegrationRejectsSubscriberOverMaxClients(t *testing.T) {
	port := freeUDPPort(t)
	server, err := NewServer(Config{
		StreamID:   "stream-1",
		BindHost:   "127.0.0.1",
		Port:       port,
		Latency:    50 * time.Millisecond,
		MaxClients: 1,
		Logger:     testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		server.Shutdown()
		if err := server.Wait(); err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	}()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn1 := dialSubscriber(t, addr, "read:stream-1")
	defer func() {
		if err := conn1.Close(); err != nil {
			t.Fatalf("conn1 Close() error = %v", err)
		}
	}()
	eventually(t, func() bool {
		return server.ClientCount() == 1
	}, "subscriber count to reach 1")
	if conn2, err := dialSubscriberErr(addr, "read:stream-1"); err == nil {
		defer func() {
			if closeErr := conn2.Close(); closeErr != nil {
				t.Fatalf("conn2 Close() error = %v", closeErr)
			}
		}()
		t.Fatal("second subscriber connected despite MaxClients=1")
	}
}
func newQueueTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := NewServer(Config{
		Port:   9000,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}
func (s *Server) addQueueOnlySubscriber(t *testing.T) *subscriber {
	t.Helper()
	sub := &subscriber{
		conn: newFakeConn(),
		ch:   make(chan []byte, s.cfg.QueueChunks),
		done: make(chan struct{}),
	}
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	s.mu.Unlock()
	return sub
}
func dialSubscriber(t *testing.T, addr, streamID string) srt.Conn {
	t.Helper()
	conn, err := dialSubscriberErr(addr, streamID)
	if err != nil {
		t.Fatalf("Dial(%s) error = %v", addr, err)
	}
	return conn
}
func dialSubscriberErr(addr, streamID string) (srt.Conn, error) {
	cfg := srt.DefaultConfig()
	cfg.ConnectionTimeout = time.Second
	cfg.Latency = 50 * time.Millisecond
	cfg.StreamId = streamID
	return srt.Dial("srt", addr, cfg)
}
func readSRT(t *testing.T, conn srt.Conn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	return string(buf[:n])
}
func freeUDPPort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ResolveUDPAddr() error = %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("ListenUDP() error = %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("UDP Close() error = %v", err)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}
func eventually(t *testing.T, fn func() bool, desc string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

type fakeRequest struct {
	streamID        string
	encrypted       bool
	passphrase      string
	passphraseErr   error
	passphraseCalls int
}

func (r *fakeRequest) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}
}
func (r *fakeRequest) Version() uint32      { return 5 }
func (r *fakeRequest) StreamId() string     { return r.streamID }
func (r *fakeRequest) SocketId() uint32     { return 1 }
func (r *fakeRequest) PeerSocketId() uint32 { return 2 }
func (r *fakeRequest) IsEncrypted() bool    { return r.encrypted }
func (r *fakeRequest) SetPassphrase(passphrase string) error {
	r.passphraseCalls++
	r.passphrase = passphrase
	return r.passphraseErr
}
func (r *fakeRequest) SetRejectionReason(srt.RejectionReason) {}
func (r *fakeRequest) Accept() (srt.Conn, error)              { return newFakeConn(), nil }
func (r *fakeRequest) Reject(srt.RejectionReason)             {}

type fakeConn struct {
	mu        sync.Mutex
	closeOnce sync.Once
	closed    atomic.Bool
	writes    [][]byte
}

func newFakeConn() *fakeConn {
	return &fakeConn{}
}
func (c *fakeConn) Read([]byte) (int, error)           { return 0, io.EOF }
func (c *fakeConn) ReadPacket() (packet.Packet, error) { return nil, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.EOF
	}
	buf := bytes.Clone(p)
	c.mu.Lock()
	c.writes = append(c.writes, buf)
	c.mu.Unlock()
	return len(p), nil
}
func (c *fakeConn) WritePacket(packet.Packet) error { return nil }
func (c *fakeConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
	})
	return nil
}
func (c *fakeConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000}
}
func (c *fakeConn) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}
}
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeConn) SocketId() uint32                 { return 1 }
func (c *fakeConn) PeerSocketId() uint32             { return 2 }
func (c *fakeConn) StreamId() string                 { return "" }
func (c *fakeConn) Stats(*srt.Statistics)            {}
func (c *fakeConn) Version() uint32                  { return 5 }
func (c *fakeConn) isClosed() bool {
	return c.closed.Load()
}
