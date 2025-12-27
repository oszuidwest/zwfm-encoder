package main

import (
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

type loginData struct {
	Error     bool
	CSRFToken string
	Version   string
	Year      int
}

type indexData struct {
	Version string
	Year    int
}

// Server is an HTTP server that provides the web interface for the audio encoder.
type Server struct {
	config   *config.Config
	encoder  *encoder.Encoder
	sessions *server.SessionManager
	commands *server.CommandHandler
	version  *VersionChecker
}

// NewServer returns a new Server configured with the provided config and encoder.
func NewServer(cfg *config.Config, enc *encoder.Encoder) *Server {
	sessions := server.NewSessionManager()
	commands := server.NewCommandHandler(cfg, enc)

	return &Server{
		config:   cfg,
		encoder:  enc,
		sessions: sessions,
		commands: commands,
		version:  NewVersionChecker(),
	}
}

// handleWebSocket handles bidirectional WebSocket communication for real-time updates.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := server.UpgradeConnection(w, r)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer util.SafeCloseFunc(conn, "WebSocket connection")()

	statusUpdate := make(chan bool, 1)
	done := make(chan bool)

	go func() {
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
			s.commands.Handle(cmd, conn, func() {
				select {
				case statusUpdate <- true:
				default:
				}
			})
		}
	}()

	levelsTicker := time.NewTicker(100 * time.Millisecond) // 10 fps for VU meter
	statusTicker := time.NewTicker(3 * time.Second)
	defer levelsTicker.Stop()
	defer statusTicker.Stop()

	sendStatus := func() error {
		cfg := s.config.Snapshot()
		status := s.encoder.Status()
		status.OutputCount = len(cfg.Outputs)

		return conn.WriteJSON(types.WSStatusResponse{
			Type:             "status",
			Encoder:          status,
			Outputs:          cfg.Outputs,
			OutputStatus:     s.encoder.AllOutputStatuses(),
			Devices:          audio.ListDevices(),
			SilenceThreshold: cfg.SilenceThreshold,
			SilenceDuration:  cfg.SilenceDuration,
			SilenceRecovery:  cfg.SilenceRecovery,
			SilenceWebhook:   cfg.WebhookURL,
			SilenceLogPath:   cfg.LogPath,
			EmailSMTPHost:    cfg.EmailSMTPHost,
			EmailSMTPPort:    cfg.EmailSMTPPort,
			EmailFromName:    cfg.EmailFromName,
			EmailUsername:    cfg.EmailUsername,
			EmailRecipients:  cfg.EmailRecipients,
			Settings: types.WSSettings{
				AudioInput: cfg.AudioInput,
				Platform:   runtime.GOOS,
			},
			Version: s.version.Info(),
		})
	}

	if err := sendStatus(); err != nil {
		return
	}

	for {
		select {
		case <-done:
			return
		case <-statusUpdate:
			if err := sendStatus(); err != nil {
				return
			}
		case <-levelsTicker.C:
			if err := conn.WriteJSON(types.WSLevelsResponse{
				Type:   "levels",
				Levels: s.encoder.AudioLevels(),
			}); err != nil {
				return
			}
		case <-statusTicker.C:
			if err := sendStatus(); err != nil {
				return
			}
		}
	}
}

// SetupRoutes returns an [http.Handler] configured with all application routes.
func (s *Server) SetupRoutes() http.Handler {
	mux := http.NewServeMux()
	cfg := s.config.Snapshot()
	auth := s.sessions.AuthMiddleware(cfg.WebUser, cfg.WebPassword)

	// Public routes (no auth required)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)

	// Public static assets (needed for login page styling)
	mux.HandleFunc("/style.css", s.handlePublicStatic)
	mux.HandleFunc("/icons.js", s.handlePublicStatic)
	mux.HandleFunc("/favicon.svg", s.handlePublicStatic)

	// Protected routes
	mux.HandleFunc("/ws", auth(s.handleWebSocket))
	mux.HandleFunc("/", auth(s.handleStatic))

	return securityHeaders(mux)
}

// securityHeaders returns middleware that adds security headers to responses.
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

	data := loginData{
		Version:   Version,
		Year:      time.Now().Year(),
		CSRFToken: s.sessions.CreateCSRFToken(),
	}

	if r.Method == http.MethodPost {
		csrfToken := r.FormValue("csrf_token")
		if !s.sessions.ValidateCSRFToken(csrfToken) {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		username := r.FormValue("username")
		password := r.FormValue("password")
		cfg := s.config.Snapshot()

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

// handleLogout handles logout by clearing the session and redirecting.
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
	"/favicon.svg": {
		contentType: "image/svg+xml",
		content:     faviconSVG,
		name:        "favicon.svg",
	},
}

// handleStatic handles requests for embedded static web interface files.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}

	// Serve index.html with dynamic placeholders.
	if path == "/index.html" {
		w.Header().Set("Content-Type", "text/html")
		if err := indexTmpl.Execute(w, indexData{
			Version: Version,
			Year:    time.Now().Year(),
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

// Start begins listening and serving HTTP requests on the configured port.
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
