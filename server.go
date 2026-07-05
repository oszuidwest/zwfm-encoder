package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	"github.com/oszuidwest/zwfm-encoder/internal/server"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
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
	config     *config.Config
	encoder    *encoder.Encoder
	sessions   *server.SessionManager
	version    *VersionChecker
	httpServer *http.Server

	// WebSocket broadcast channels
	wsClients       map[chan any]struct{}
	wsClientsMu     sync.RWMutex
	wsBroadcastOnce sync.Once
}

// rawWSMessage carries pre-marshaled JSON so the broadcaster serializes each
// periodic payload once instead of once per connected client.
type rawWSMessage []byte

// NewServer creates a Server with the given configuration and encoder.
func NewServer(cfg *config.Config, enc *encoder.Encoder) *Server {
	return &Server{
		config:    cfg,
		encoder:   enc,
		sessions:  server.NewSessionManager(),
		version:   NewVersionChecker(),
		wsClients: make(map[chan any]struct{}),
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

	// Register this client for broadcasts.
	// Cleanup: unregister before closing so the channel is removed from the map
	// while broadcastConfigChanged holds RLock during sends, preventing send-on-closed-channel.
	s.registerWSClient(send)
	defer func() {
		s.unregisterWSClient(send)
		close(send)
	}()

	// Writer goroutine - sole writer to the connection
	go s.runWebSocketWriter(conn, send)

	// Reader goroutine - keeps connection alive
	go s.runWebSocketReader(conn, done)

	// One process-wide broadcaster feeds all clients; started on first use.
	s.wsBroadcastOnce.Do(func() { go s.runWSBroadcaster() })

	// Send an initial status snapshot, then block until the reader detects
	// disconnect; the broadcaster delivers all further periodic updates.
	select {
	case send <- s.buildWSRuntime():
	case <-done:
	}
	<-done
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

// broadcastConfigChanged notifies all connected WebSocket clients that config has changed.
func (s *Server) broadcastConfigChanged() {
	s.wsClientsMu.RLock()
	defer s.wsClientsMu.RUnlock()

	msg := map[string]string{"type": "config_changed"}
	for ch := range s.wsClients {
		select {
		case ch <- msg:
		default:
			// Channel full, skip this client
		}
	}
}

// runWebSocketWriter is the sole writer goroutine for a WebSocket connection.
func (s *Server) runWebSocketWriter(conn server.WebSocketConn, send <-chan any) {
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Debug("WebSocket close error", "error", err)
		}
	}()
	for msg := range send {
		var err error
		if raw, ok := msg.(rawWSMessage); ok {
			err = conn.WriteMessage(server.TextMessage, raw)
		} else {
			err = conn.WriteJSON(msg)
		}
		if err != nil {
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

// runWSBroadcaster builds each periodic WebSocket payload once and fans the
// marshaled bytes out to every connected client, instead of every client
// assembling and marshaling its own identical copy. Started lazily on the
// first WebSocket connection and runs for the process lifetime; it idles
// cheaply while no clients are connected.
func (s *Server) runWSBroadcaster() {
	levelsTicker := time.NewTicker(100 * time.Millisecond) // 10 fps for VU meters
	statusTicker := time.NewTicker(3 * time.Second)        // Status updates every 3s
	defer levelsTicker.Stop()
	defer statusTicker.Stop()

	for {
		select {
		case <-levelsTicker.C:
			s.broadcastShared(func() any {
				return types.WSLevelsResponse{Type: "levels", Levels: s.encoder.AudioLevels()}
			})
		case <-statusTicker.C:
			s.broadcastShared(func() any { return s.buildWSRuntime() })
		}
	}
}

// broadcastShared marshals the payload once and queues the bytes on every
// client channel. The payload is only built when at least one client is
// connected. Full client channels are skipped, matching broadcastConfigChanged.
func (s *Server) broadcastShared(build func() any) {
	s.wsClientsMu.RLock()
	clientCount := len(s.wsClients)
	s.wsClientsMu.RUnlock()
	if clientCount == 0 {
		return
	}

	data, err := json.Marshal(build())
	if err != nil {
		slog.Error("failed to marshal WebSocket payload", "error", err)
		return
	}

	s.wsClientsMu.RLock()
	defer s.wsClientsMu.RUnlock()
	for ch := range s.wsClients {
		select {
		case ch <- rawWSMessage(data):
		default:
			// Channel full, skip this client
		}
	}
}

func (s *Server) buildWSRuntime() types.WSRuntimeStatus {
	cfg := s.config.Snapshot()
	status := s.encoder.Status()
	status.StreamCount = len(cfg.Streams)

	return types.WSRuntimeStatus{
		Type:               "status",
		FFmpegAvailable:    s.encoder.FFmpegAvailable(),
		SRTAvailable:       s.encoder.SRTAvailable(),
		SRTError:           s.encoder.SRTErrorMessage(),
		RecordingAvailable: s.encoder.RecordingAvailable(),
		Encoder:            status,
		StreamStatus:       s.encoder.StreamStatuses(cfg.Streams),
		RecorderStatuses:   s.encoder.RecorderStatuses(),
		GraphSecretExpiry:  s.encoder.GraphSecretExpiry(),
		Version:            s.version.Info(),
		EventSeq:           s.encoder.EventSeq(),
	}
}

// SetupRoutes configures and returns the HTTP handler with all routes.
func (s *Server) SetupRoutes() http.Handler {
	mux := http.NewServeMux()
	auth := s.sessions.AuthMiddleware()

	// Public routes (no auth required)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)

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

// securityHeaders wraps a handler to add security headers to responses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// handlePublicStatic serves style.css and icons.js without authentication.
func (s *Server) handlePublicStatic(w http.ResponseWriter, r *http.Request) {
	if !serveStaticFile(w, r, r.URL.Path) {
		http.NotFound(w, r)
	}
}

// handleFavicon generates an SVG favicon with the configured station color.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	w.Header().Set("Content-Type", "image/svg+xml")
	if err := faviconTmpl.Execute(w, struct{ Color string }{Color: cfg.StationColorLight}); err != nil {
		slog.Error("failed to render favicon", "error", err)
	}
}

// staticETag identifies the embedded asset build. Assets only change with a
// new binary, so release builds let clients revalidate with a conditional
// request instead of re-downloading ~100 KB of scripts on every page load.
// Dev builds share one version string, so they skip caching entirely.
var staticETag = func() string {
	if Version == "dev" {
		return ""
	}
	return `"` + Version + `"`
}()

// serveStaticFile reports whether the path was found and served.
func serveStaticFile(w http.ResponseWriter, r *http.Request, path string) bool {
	file, ok := staticFiles[path]
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", file.contentType)
	if staticETag != "" {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", staticETag)
		if r.Header.Get("If-None-Match") == staticETag {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	if _, err := w.Write(file.content); err != nil {
		slog.Error("failed to write static file", "file", path, "error", err)
	}
	return true
}

// handleLogin serves the login page and processes form submissions.
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
		//nolint:gosec // CSS from validated hex colors
		PrimaryCSS: template.CSS(generateBrandCSS(cfg.StationColorLight, cfg.StationColorDark)),
	}

	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
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

// staticFile holds an embedded static file with its content type and data.
// Content is converted to bytes once so serving does not copy per request.
type staticFile struct {
	contentType string
	content     []byte
}

var staticFiles = map[string]staticFile{
	"/style.css":       {contentType: "text/css", content: []byte(styleCSS)},
	"/app.js":          {contentType: "application/javascript", content: []byte(appJS)},
	"/eventHelpers.js": {contentType: "application/javascript", content: []byte(eventHelpersJS)},
	"/icons.js":        {contentType: "application/javascript", content: []byte(iconsJS)},
	"/alpine.min.js":   {contentType: "application/javascript", content: []byte(alpineJS)},
	// favicon.svg is served dynamically via handleFavicon
}

// handleStatic serves embedded static files for authenticated users.
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
			//nolint:gosec // CSS from validated hex colors
			PrimaryCSS: template.CSS(generateBrandCSS(cfg.StationColorLight, cfg.StationColorDark)),
		}); err != nil {
			slog.Error("failed to write index.html", "error", err)
		}
		return
	}

	if serveStaticFile(w, r, path) {
		return
	}

	http.NotFound(w, r)
}

// apiKeyAuth wraps a handler with API key authentication.
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

	if status, errMsg := s.doRecorderAction(recorderID, isStart); errMsg != "" {
		s.writeError(w, status, errMsg)
		return
	}

	result := "recording_stopped"
	if isStart {
		result = "recording_started"
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"status":      result,
		"recorder_id": recorderID,
	})
}

// Start begins the HTTP server and returns it for graceful shutdown.
func (s *Server) Start() {
	addr := fmt.Sprintf(":%d", s.config.Snapshot().WebPort)
	slog.Info("starting web server", "addr", addr)

	s.httpServer = &http.Server{
		Addr:           addr,
		Handler:        s.SetupRoutes(),
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()
}

// Stop halts the version checker and gracefully shuts down the HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	s.version.Stop()
	return s.httpServer.Shutdown(ctx)
}
