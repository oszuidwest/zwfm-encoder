// Package srtfanout provides a bounded byte fan-out over SRT subscribers.
package srtfanout

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	srt "github.com/datarhei/gosrt"
)

const (
	// DefaultLatency is the SRT latency used for listener subscribers.
	DefaultLatency = 300 * time.Millisecond
	// DefaultMaxClients limits concurrent subscribers until this becomes configurable.
	DefaultMaxClients = 16
	// defaultQueueChunks is the bounded chunk queue per subscriber.
	defaultQueueChunks = 2
)

// Config describes a fan-out SRT listener.
type Config struct {
	StreamID   string
	BindHost   string
	Port       int
	Password   string
	Latency    time.Duration
	MaxClients int
	Logger     *slog.Logger
}

// Server owns one GoSRT listener and fans encoded bytes to subscribers.
type Server struct {
	cfg Config

	server *srt.Server

	startMu   sync.Mutex
	started   bool
	serveDone chan struct{}
	serveErr  error

	mu          sync.RWMutex
	stopping    bool
	subscribers map[*subscriber]struct{}

	subscriberWg sync.WaitGroup
	stopOnce     sync.Once
	clientCount  atomic.Int64
}

type subscriber struct {
	conn srt.Conn
	ch   chan []byte

	done      chan struct{}
	closeOnce sync.Once
	drops     int64
}

// NewServer validates cfg and returns a stopped fan-out server.
//
//nolint:gocritic // Keep the public constructor value-based so callers can pass short-lived configs safely.
func NewServer(cfg Config) (*Server, error) {
	if cfg.BindHost == "" {
		cfg.BindHost = "0.0.0.0"
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("port: must be between 1 and 65535")
	}
	if cfg.Latency <= 0 {
		cfg.Latency = DefaultLatency
	}
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = DefaultMaxClients
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Server{
		cfg:         cfg,
		subscribers: make(map[*subscriber]struct{}),
	}, nil
}

// Start binds the SRT listener synchronously and serves subscribers in the background.
func (s *Server) Start() error {
	s.startMu.Lock()
	defer s.startMu.Unlock()

	if s.started {
		return fmt.Errorf("srt fanout already started")
	}

	config := srt.DefaultConfig()
	config.Latency = s.cfg.Latency
	config.PBKeylen = 16
	config.TransmissionType = "live"

	s.server = &srt.Server{
		Addr:            net.JoinHostPort(s.cfg.BindHost, strconv.Itoa(s.cfg.Port)),
		Config:          &config,
		HandleConnect:   s.handleConnect,
		HandleSubscribe: s.handleSubscribe,
	}

	if err := s.server.Listen(); err != nil {
		return fmt.Errorf("listen srt fanout: %w", err)
	}

	s.serveDone = make(chan struct{})
	s.started = true
	go s.serve()

	return nil
}

func (s *Server) serve() {
	err := s.server.Serve()
	if errors.Is(err, srt.ErrServerClosed) {
		err = nil
	}
	s.serveErr = err
	close(s.serveDone)
}

// Shutdown stops accepting subscribers and closes all active subscriber connections.
func (s *Server) Shutdown() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		subscribers := slices.Collect(maps.Keys(s.subscribers))
		s.mu.Unlock()

		if s.server != nil {
			s.server.Shutdown()
		}
		for _, sub := range subscribers {
			sub.stop()
		}
	})
}

// Wait blocks until the listener and subscriber writer goroutines have exited.
func (s *Server) Wait() error {
	s.startMu.Lock()
	serveDone := s.serveDone
	s.startMu.Unlock()

	if serveDone != nil {
		<-serveDone
	}
	s.subscriberWg.Wait()
	return s.serveErr
}

// Write fans chunk out to every current subscriber without blocking on slow clients.
func (s *Server) Write(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	// enqueue is non-blocking, so the RLock can span the fan-out loop instead
	// of snapshotting subscribers into a per-chunk slice on the audio hot path.
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.subscribers) == 0 {
		return
	}

	buf := bytes.Clone(chunk)

	for sub := range s.subscribers {
		if dropped := sub.enqueue(buf); dropped {
			s.logSlowSubscriberDrop(sub)
		}
	}
}

// ClientCount returns the number of active subscriber connections.
func (s *Server) ClientCount() int64 {
	return s.clientCount.Load()
}

func (s *Server) handleConnect(req srt.ConnRequest) srt.ConnType {
	if s.clientCount.Load() >= int64(s.cfg.MaxClients) {
		s.cfg.Logger.Warn("srt subscriber rejected: max clients reached",
			"stream_id", s.cfg.StreamID,
			"remote_addr", req.RemoteAddr(),
			"max_clients", s.cfg.MaxClients)
		return srt.REJECT
	}

	if s.cfg.Password == "" {
		if req.IsEncrypted() {
			s.cfg.Logger.Warn("srt subscriber rejected: encrypted request without configured password",
				"stream_id", s.cfg.StreamID,
				"remote_addr", req.RemoteAddr())
			return srt.REJECT
		}
		return srt.SUBSCRIBE
	}

	if !req.IsEncrypted() {
		s.cfg.Logger.Warn("srt subscriber rejected: encryption required",
			"stream_id", s.cfg.StreamID,
			"remote_addr", req.RemoteAddr())
		return srt.REJECT
	}
	if err := req.SetPassphrase(s.cfg.Password); err != nil {
		s.cfg.Logger.Warn("srt subscriber rejected: passphrase failed",
			"stream_id", s.cfg.StreamID,
			"remote_addr", req.RemoteAddr(),
			"error", err)
		return srt.REJECT
	}

	return srt.SUBSCRIBE
}

func (s *Server) handleSubscribe(conn srt.Conn) {
	sub := &subscriber{
		conn: conn,
		ch:   make(chan []byte, defaultQueueChunks),
		done: make(chan struct{}),
	}
	if !s.addSubscriber(sub) {
		_ = conn.Close()
		return
	}

	defer s.subscriberWg.Done()
	defer s.removeSubscriber(sub)

	for {
		select {
		case <-sub.done:
			return
		case chunk := <-sub.ch:
			if _, err := conn.Write(chunk); err != nil {
				s.cfg.Logger.Warn("srt subscriber write failed",
					"stream_id", s.cfg.StreamID,
					"remote_addr", conn.RemoteAddr(),
					"error", err)
				return
			}
		}
	}
}

func (s *Server) addSubscriber(sub *subscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopping {
		return false
	}

	s.subscriberWg.Add(1)
	s.subscribers[sub] = struct{}{}
	s.clientCount.Add(1)
	s.cfg.Logger.Info("srt subscriber connected",
		"stream_id", s.cfg.StreamID,
		"remote_addr", sub.conn.RemoteAddr(),
		"clients", s.clientCount.Load())
	return true
}

func (s *Server) removeSubscriber(sub *subscriber) {
	removed := false
	s.mu.Lock()
	if _, ok := s.subscribers[sub]; ok {
		delete(s.subscribers, sub)
		s.clientCount.Add(-1)
		removed = true
	}
	s.mu.Unlock()

	sub.stop()
	if removed {
		s.cfg.Logger.Info("srt subscriber disconnected",
			"stream_id", s.cfg.StreamID,
			"remote_addr", sub.conn.RemoteAddr(),
			"clients", s.clientCount.Load())
	}
}

func (s *Server) logSlowSubscriberDrop(sub *subscriber) {
	drops := atomic.LoadInt64(&sub.drops)
	if drops == 1 || drops%100 == 0 {
		s.cfg.Logger.Warn("srt subscriber queue full, dropping stale chunk",
			"stream_id", s.cfg.StreamID,
			"remote_addr", sub.conn.RemoteAddr(),
			"drops", drops)
	}
}

func (sub *subscriber) enqueue(chunk []byte) bool {
	// Server.Write currently has one active producer: the stdout reader for the
	// current encoder run. Concurrent producers are memory-safe, but can race the
	// final non-blocking send and skip their freshest chunk under contention.
	select {
	case sub.ch <- chunk:
		return false
	case <-sub.done:
		return false
	default:
	}

	dropped := false
	select {
	case <-sub.ch:
		atomic.AddInt64(&sub.drops, 1)
		dropped = true
	default:
	}

	select {
	case sub.ch <- chunk:
	default:
	}
	return dropped
}

func (sub *subscriber) stop() {
	sub.closeOnce.Do(func() {
		close(sub.done)
		_ = sub.conn.Close()
	})
}
