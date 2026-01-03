package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
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

// Server is an HTTP server that provides the web interface for the audio encoder.
type Server struct {
	config          *config.Config
	encoder         *encoder.Encoder
	sessions        *server.SessionManager
	commands        *server.CommandHandler
	version         *VersionChecker
	ffmpegAvailable bool
}

// NewServer returns a new Server configured with the provided config and encoder.
func NewServer(cfg *config.Config, enc *encoder.Encoder, ffmpegAvailable bool) *Server {
	sessions := server.NewSessionManager()
	commands := server.NewCommandHandler(cfg, enc, ffmpegAvailable)

	return &Server{
		config:          cfg,
		encoder:         enc,
		sessions:        sessions,
		commands:        commands,
		version:         NewVersionChecker(),
		ffmpegAvailable: ffmpegAvailable,
	}
}

// handleWebSocket handles bidirectional WebSocket communication for real-time updates.
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
	statusUpdate := make(chan struct{}, 1)

	// Writer goroutine - sole writer to the connection
	go s.runWebSocketWriter(conn, send)

	// Reader goroutine - handles incoming commands
	go s.runWebSocketReader(conn, send, done, statusUpdate)

	s.runWebSocketEventLoop(send, done, statusUpdate)
}

// runWebSocketWriter writes messages from the send channel to the connection.
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

// runWebSocketReader reads commands from the connection and dispatches them.
func (s *Server) runWebSocketReader(conn server.WebSocketConn, send chan<- any, done, statusUpdate chan<- struct{}) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in WebSocket reader", "panic", r)
		}
		close(done)
	}()

	for {
		var cmd server.WSCommand
		if err := conn.ReadJSON(&cmd); err != nil {
			return
		}
		s.commands.Handle(cmd, send, func() {
			select {
			case statusUpdate <- struct{}{}:
			default:
			}
		})
	}
}

// runWebSocketEventLoop handles periodic status and level updates.
func (s *Server) runWebSocketEventLoop(send chan any, done, statusUpdate <-chan struct{}) {
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
	if !trySend(s.buildWSStatus()) {
		close(send)
		return
	}

	for {
		select {
		case <-done:
			close(send)
			return
		case <-statusUpdate:
			if !trySend(s.buildWSStatus()) {
				close(send)
				return
			}
		case <-levelsTicker.C:
			if !trySend(types.WSLevelsResponse{Type: "levels", Levels: s.encoder.AudioLevels()}) {
				close(send)
				return
			}
		case <-statusTicker.C:
			if !trySend(s.buildWSStatus()) {
				close(send)
				return
			}
		}
	}
}

// buildWSStatus returns the current WebSocket status response.
func (s *Server) buildWSStatus() types.WSStatusResponse {
	cfg := s.config.Snapshot()
	status := s.encoder.Status()
	status.OutputCount = len(cfg.Outputs)

	return types.WSStatusResponse{
		Type:              "status",
		FFmpegAvailable:   s.ffmpegAvailable,
		Encoder:           status,
		Outputs:           cfg.Outputs,
		OutputStatus:      s.encoder.AllOutputStatuses(cfg.Outputs),
		Recorders:         cfg.Recorders,
		RecorderStatuses:  s.encoder.AllRecorderStatuses(),
		RecordingAPIKey:   cfg.RecordingAPIKey,
		Devices:           audio.ListDevices(),
		SilenceThreshold:  cfg.SilenceThreshold,
		SilenceDurationMs: cfg.SilenceDurationMs,
		SilenceRecoveryMs: cfg.SilenceRecoveryMs,
		SilenceWebhook:    cfg.WebhookURL,
		SilenceLogPath:    cfg.LogPath,
		GraphTenantID:     cfg.GraphTenantID,
		GraphClientID:     cfg.GraphClientID,
		GraphFromAddress:  cfg.GraphFromAddress,
		GraphRecipients:   cfg.GraphRecipients,
		GraphSecretExpiry: s.encoder.GraphSecretExpiry(),
		SilenceDump: types.SilenceDumpConfig{
			Enabled:       cfg.SilenceDumpEnabled,
			RetentionDays: cfg.SilenceDumpRetentionDays,
		},
		Settings: types.WSSettings{
			AudioInput: cfg.AudioInput,
			Platform:   runtime.GOOS,
		},
		Version: s.version.Info(),
	}
}

// SetupRoutes returns an [http.Handler] configured with all application routes.
func (s *Server) SetupRoutes() http.Handler {
	mux := http.NewServeMux()
	auth := s.sessions.AuthMiddleware()

	// Public routes (no auth required)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)

	// Public static assets (needed for login page styling)
	mux.HandleFunc("/style.css", s.handlePublicStatic)
	mux.HandleFunc("/icons.js", s.handlePublicStatic)
	mux.HandleFunc("/favicon.svg", s.handleFavicon)

	// Recording API routes (API key auth)
	mux.HandleFunc("/api/recordings/start", s.apiKeyAuth(s.handleStartRecording))
	mux.HandleFunc("/api/recordings/stop", s.apiKeyAuth(s.handleStopRecording))

	// Protected routes
	mux.HandleFunc("/ws", auth(s.handleWebSocket))
	mux.HandleFunc("/", auth(s.handleStatic))

	return securityHeaders(mux)
}

// securityHeaders returns middleware that wraps handlers with security headers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// handlePublicStatic handles requests for static files without authentication.
func (s *Server) handlePublicStatic(w http.ResponseWriter, r *http.Request) {
	if !serveStaticFile(w, r.URL.Path) {
		http.NotFound(w, r)
	}
}

// handleFavicon serves the favicon with the configured station color.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	cfg := s.config.Snapshot()
	w.Header().Set("Content-Type", "image/svg+xml")
	if err := faviconTmpl.Execute(w, struct{ Color string }{Color: cfg.StationColorLight}); err != nil {
		slog.Error("failed to render favicon", "error", err)
	}
}

// serveStaticFile serves a static file by path and reports whether it was found.
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

// handleLogin handles login page display and form submission.
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

// handleLogout handles user logout requests.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// staticFile is an embedded static file with content type and data.
type staticFile struct {
	contentType string
	content     string
	name        string
}

// staticFiles is a map from URL paths to static file definitions.
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

// handleStatic handles requests for embedded static web interface files.
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

// apiKeyAuth returns middleware for API key authentication.
func (s *Server) apiKeyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := s.config.GetRecordingAPIKey()
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

// handleStartRecording handles POST /api/recordings/start?recorder_id=xxx.
func (s *Server) handleStartRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	recorderID := r.URL.Query().Get("recorder_id")
	if recorderID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "recorder_id is required"}); err != nil {
			slog.Error("failed to encode error response", "error", err)
		}
		return
	}

	if err := s.encoder.StartRecorder(recorderID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); err != nil {
			slog.Error("failed to encode error response", "error", err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "recording_started", "recorder_id": recorderID}); err != nil {
		slog.Error("failed to encode success response", "error", err)
	}
}

// handleStopRecording handles POST /api/recordings/stop?recorder_id=xxx.
func (s *Server) handleStopRecording(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	recorderID := r.URL.Query().Get("recorder_id")
	if recorderID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "recorder_id is required"}); err != nil {
			slog.Error("failed to encode error response", "error", err)
		}
		return
	}

	if err := s.encoder.StopRecorder(recorderID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); err != nil {
			slog.Error("failed to encode error response", "error", err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "recording_stopped", "recorder_id": recorderID}); err != nil {
		slog.Error("failed to encode success response", "error", err)
	}
}

// Start begins the HTTP server.
// Returns an *http.Server that can be used for graceful shutdown.
func (s *Server) Start() *http.Server {
	addr := fmt.Sprintf(":%d", s.config.Snapshot().WebPort)
	slog.Info("starting web server", "addr", addr)

	srv := &http.Server{
		Addr:    addr,
		Handler: s.SetupRoutes(),
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	return srv
}
