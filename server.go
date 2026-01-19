package main

import (
	"crypto/subtle"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	"github.com/oszuidwest/zwfm-encoder/internal/server"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

var loginTmpl = template.Must(template.New("login").Parse(loginHTML))
var indexTmpl = template.Must(template.New("index").Parse(indexHTML))
var faviconTmpl = template.Must(template.New("favicon").Parse(faviconSVG))

type loginData struct {
	Error       bool
	CSRFToken   string
	Version     string
	Year        int
	StationName string
	PrimaryCSS  template.CSS
}

type indexData struct {
	Version     string
	Year        int
	StationName string
	PrimaryCSS  template.CSS
}

// Server handles HTTP requests and WebSocket connections.
type Server struct {
	config          *config.Config
	encoder         *encoder.Encoder
	sessions        *server.SessionManager
	version         *VersionChecker
	ffmpegAvailable bool

	// WebSocket broadcast channels
	wsClients   map[chan any]struct{}
	wsClientsMu sync.RWMutex
}

func NewServer(cfg *config.Config, enc *encoder.Encoder, ffmpegAvailable bool) *Server {
	return &Server{
		config:          cfg,
		encoder:         enc,
		sessions:        server.NewSessionManager(),
		version:         NewVersionChecker(),
		ffmpegAvailable: ffmpegAvailable,
		wsClients:       make(map[chan any]struct{}),
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := server.UpgradeConnection(w, r)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}

	// Create buffered send channel for thread-safe writes.
	// Only the writer goroutine writes to the connection, preventing race conditions.
	send := make(chan any, 16)
	done := make(chan struct{})

	// Register this client for broadcasts
	s.registerWSClient(send)
	defer s.unregisterWSClient(send)

	// Writer goroutine - sole writer to the connection
	go s.runWebSocketWriter(conn, send)

	// Reader goroutine - keeps connection alive
	go s.runWebSocketReader(conn, done)

	s.runWebSocketEventLoop(send, done)
}

func (s *Server) registerWSClient(send chan any) {
	s.wsClientsMu.Lock()
	s.wsClients[send] = struct{}{}
	s.wsClientsMu.Unlock()
}

func (s *Server) unregisterWSClient(send chan any) {
	s.wsClientsMu.Lock()
	delete(s.wsClients, send)
	s.wsClientsMu.Unlock()
}

func (s *Server) broadcastConfigChanged() {
	// Copy client channels while holding lock to avoid race conditions
	s.wsClientsMu.RLock()
	clients := slices.Collect(maps.Keys(s.wsClients))
	s.wsClientsMu.RUnlock()

	// Send to all clients outside the lock
	msg := map[string]string{"type": "config_changed"}
	for _, ch := range clients {
		select {
		case ch <- msg:
		default:
			// Channel full, skip this client
		}
	}
}

func (s *Server) runWebSocketWriter(conn server.WebSocketConn, send <-chan any) {
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Debug("WebSocket close error", "error", err)
		}
	}()
	for msg := range send {
		if err := conn.WriteJSON(msg); err != nil {
			return
		}
	}
}

func (s *Server) runWebSocketReader(conn server.WebSocketConn, done chan<- struct{}) {
	defer close(done)

	for {
		// Read messages to keep connection alive and detect disconnects
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (s *Server) runWebSocketEventLoop(send chan any, done <-chan struct{}) {
	levelsTicker := time.NewTicker(100 * time.Millisecond)  // 10 fps for VU meters
	statusTicker := time.NewTicker(3000 * time.Millisecond) // Status updates every 3s
	defer levelsTicker.Stop()
	defer statusTicker.Stop()

	// trySend attempts to send a message, returning false if done is closed
	trySend := func(msg any) bool {
		select {
		case send <- msg:
			return true
		case <-done:
			return false
		}
	}

	// Send initial status
	if !trySend(s.buildWSRuntime()) {
		close(send)
		return
	}

	for {
		select {
		case <-done:
			close(send)
			return
		case <-levelsTicker.C:
			if !trySend(types.WSLevelsResponse{Type: "levels", Levels: s.encoder.AudioLevels()}) {
				close(send)
				return
			}
		case <-statusTicker.C:
			if !trySend(s.buildWSRuntime()) {
				close(send)
				return
			}
		}
	}
}

func (s *Server) buildWSRuntime() types.WSRuntimeStatus {
	cfg := s.config.Snapshot()
	status := s.encoder.Status()
	status.StreamCount = len(cfg.Streams)

	return types.WSRuntimeStatus{
		Type:              "status",
		FFmpegAvailable:   s.ffmpegAvailable,
		Encoder:           status,
		StreamStatus:      s.encoder.StreamStatuses(cfg.Streams),
		RecorderStatuses:  s.encoder.RecorderStatuses(),
		GraphSecretExpiry: s.encoder.GraphSecretExpiry(),
		Version:           s.version.Info(),
	}
}

func (s *Server) SetupRoutes() http.Handler {
	mux := http.NewServeMux()
	auth := s.sessions.AuthMiddleware()

	// Public routes (no auth required)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("GET /health", s.handleHealth)

	// Public static assets (needed for login page styling)
	mux.HandleFunc("/style.css", s.handlePublicStatic)
	mux.HandleFunc("/icons.js", s.handlePublicStatic)
	mux.HandleFunc("/favicon.svg", s.handleFavicon)

	// Recording API routes (API key auth)
	mux.HandleFunc("POST /api/recordings/start", s.apiKeyAuth(s.handleExternalRecordingAction))
	mux.HandleFunc("POST /api/recordings/stop", s.apiKeyAuth(s.handleExternalRecordingAction))

	// REST API routes (session auth)
	mux.HandleFunc("GET /api/config", auth(s.handleAPIConfig))
	mux.HandleFunc("GET /api/devices", auth(s.handleAPIDevices))
	mux.HandleFunc("POST /api/settings", auth(s.handleAPISettings))

	// Stream CRUD routes
	mux.HandleFunc("GET /api/streams", auth(s.handleListStreams))
	mux.HandleFunc("POST /api/streams", auth(s.handleCreateStream))
	mux.HandleFunc("GET /api/streams/{id}", auth(s.handleGetStream))
	mux.HandleFunc("PUT /api/streams/{id}", auth(s.handleUpdateStream))
	mux.HandleFunc("DELETE /api/streams/{id}", auth(s.handleDeleteStream))

	// Recorder CRUD routes
	mux.HandleFunc("GET /api/recorders", auth(s.handleListRecorders))
	mux.HandleFunc("POST /api/recorders", auth(s.handleCreateRecorder))
	mux.HandleFunc("POST /api/recorders/test-s3", auth(s.handleTestS3))
	mux.HandleFunc("GET /api/recorders/{id}", auth(s.handleGetRecorder))
	mux.HandleFunc("PUT /api/recorders/{id}", auth(s.handleUpdateRecorder))
	mux.HandleFunc("DELETE /api/recorders/{id}", auth(s.handleDeleteRecorder))
	mux.HandleFunc("POST /api/recorders/{id}/{action}", auth(s.handleRecorderAction))

	// Notification test routes
	mux.HandleFunc("POST /api/notifications/test/webhook", auth(s.handleAPITestWebhook))
	mux.HandleFunc("POST /api/notifications/test/email", auth(s.handleAPITestEmail))
	mux.HandleFunc("POST /api/notifications/test/zabbix", auth(s.handleAPITestZabbix))

	// Recording API key management
	mux.HandleFunc("POST /api/recording/regenerate-key", auth(s.handleAPIRegenerateKey))

	// Event log
	mux.HandleFunc("GET /api/events", auth(s.handleAPIEvents))

	// Protected routes
	mux.HandleFunc("/ws", auth(s.handleWebSocket))
	mux.HandleFunc("/", auth(s.handleStatic))

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePublicStatic(w http.ResponseWriter, r *http.Request) {
	if !serveStaticFile(w, r.URL.Path) {
		http.NotFound(w, r)
	}
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	w.Header().Set("Content-Type", "image/svg+xml")
	if err := faviconTmpl.Execute(w, struct{ Color string }{Color: cfg.StationColorLight}); err != nil {
		slog.Error("failed to render favicon", "error", err)
	}
}

func serveStaticFile(w http.ResponseWriter, path string) bool {
	file, ok := staticFiles[path]
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", file.contentType)
	if _, err := w.Write([]byte(file.content)); err != nil {
		slog.Error("failed to write static file", "file", file.name, "error", err)
	}
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("encoder_session"); err == nil {
		if s.sessions.Validate(cookie.Value) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	cfg := s.config.Snapshot()
	data := loginData{
		Version:     Version,
		Year:        time.Now().Year(),
		CSRFToken:   s.sessions.CreateCSRFToken(),
		StationName: cfg.StationName,
		PrimaryCSS:  template.CSS(util.GenerateBrandCSS(cfg.StationColorLight, cfg.StationColorDark)),
	}

	if r.Method == http.MethodPost {
		csrfToken := r.FormValue("csrf_token")
		if !s.sessions.ValidateCSRFToken(csrfToken) {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		username := r.FormValue("username")
		password := r.FormValue("password")

		if s.sessions.Login(w, r, username, password, cfg.WebUser, cfg.WebPassword) {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}

		data.Error = true
		data.CSRFToken = s.sessions.CreateCSRFToken() // New token for retry
	}

	w.Header().Set("Content-Type", "text/html")
	if err := loginTmpl.Execute(w, data); err != nil {
		slog.Error("failed to render login page", "error", err)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

type staticFile struct {
	contentType string
	content     string
	name        string
}

var staticFiles = map[string]staticFile{
	"/style.css": {
		contentType: "text/css",
		content:     styleCSS,
		name:        "style.css",
	},
	"/app.js": {
		contentType: "application/javascript",
		content:     appJS,
		name:        "app.js",
	},
	"/icons.js": {
		contentType: "application/javascript",
		content:     iconsJS,
		name:        "icons.js",
	},
	"/alpine.min.js": {
		contentType: "application/javascript",
		content:     alpineJS,
		name:        "alpine.min.js",
	},
	// favicon.svg is served dynamically via handleFavicon
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}

	// Serve index.html with dynamic placeholders.
	if path == "/index.html" {
		cfg := s.config.Snapshot()
		w.Header().Set("Content-Type", "text/html")
		if err := indexTmpl.Execute(w, indexData{
			Version:     Version,
			Year:        time.Now().Year(),
			StationName: cfg.StationName,
			PrimaryCSS:  template.CSS(util.GenerateBrandCSS(cfg.StationColorLight, cfg.StationColorDark)),
		}); err != nil {
			slog.Error("failed to write index.html", "error", err)
		}
		return
	}

	if serveStaticFile(w, path) {
		return
	}

	http.NotFound(w, r)
}

func (s *Server) apiKeyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := s.config.RecordingAPIKey()
		if apiKey == "" {
			http.Error(w, "API key not configured", http.StatusServiceUnavailable)
			return
		}

		providedKey := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(providedKey), []byte(apiKey)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleExternalRecordingAction(w http.ResponseWriter, r *http.Request) {
	recorderID := r.URL.Query().Get("recorder_id")
	if recorderID == "" {
		s.writeError(w, http.StatusBadRequest, "recorder_id is required")
		return
	}

	// Determine action from path
	isStart := strings.HasSuffix(r.URL.Path, "/start")

	var err error
	var status string

	if isStart {
		if s.encoder.State() != types.StateRunning {
			s.writeError(w, http.StatusBadRequest, "Encoder must be running to start recorder")
			return
		}
		err = s.encoder.StartRecorder(recorderID)
		status = "recording_started"
	} else {
		err = s.encoder.StopRecorder(recorderID)
		status = "recording_stopped"
	}

	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]string{
		"status":      status,
		"recorder_id": recorderID,
	})
}

func (s *Server) Start() *http.Server {
	addr := fmt.Sprintf(":%d", s.config.Snapshot().WebPort)
	slog.Info("starting web server", "addr", addr)

	srv := &http.Server{
		Addr:           addr,
		Handler:        s.SetupRoutes(),
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	return srv
}
