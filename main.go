// Package main provides an audio streaming application that captures audio from
// digital input and streams to multiple SRT destinations.
//
// Usage:
//
//	encoder [-config path/to/config.json]
//
// If -config is not specified, the encoder looks for config.json in the same
// directory as the binary.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// app bundles the runtime handles that the platform shell and shutdown need.
type app struct {
	cfg        *config.Config
	encoder    *encoder.Encoder
	server     *Server
	httpServer *http.Server
}

func main() {
	// Redirect slog to a file when stderr is not attached (Windows GUI build);
	// no-op on Unix. First thing in main so even startup errors are captured.
	util.SetupLogging()

	configPath := flag.String("config", "", "Path to config file (default: config.json next to binary)")
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		slog.Info("version info", "version", Version, "commit", Commit, "build_time", BuildTime)
		return
	}

	if *configPath == "" {
		execPath, err := os.Executable()
		if err != nil {
			slog.Error("failed to get executable path", "error", err)
			os.Exit(1)
		}
		*configPath = filepath.Join(filepath.Dir(execPath), "config.json")
	}

	slog.Info("using config file", "path", *configPath)

	a, err := startApp(*configPath)
	if err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}

	runShell(a)

	slog.Info("shutting down")
	shutdown(a)
	slog.Info("shutdown complete")
}

// startApp wires config, encoder, and HTTP server. It starts the encoder (if
// FFmpeg is available) and the HTTP listener, and returns the handles for the
// platform shell and shutdown to use.
func startApp(configPath string) (*app, error) {
	cfg := config.New(configPath)
	if err := cfg.Load(); err != nil {
		return nil, err
	}

	ffmpegPath := util.ResolveFFmpegPath(cfg.FFmpegPath())
	ffmpegAvailable := ffmpegPath != ""
	if !ffmpegAvailable {
		slog.Warn("FFmpeg not found - running in degraded mode",
			"configured_path", cfg.FFmpegPath())
	} else {
		slog.Info("FFmpeg found", "path", ffmpegPath)
	}

	enc, err := encoder.New(cfg, ffmpegPath)
	if err != nil {
		return nil, err
	}
	srv := NewServer(cfg, enc, ffmpegAvailable)

	if err := enc.InitRecording(); err != nil {
		slog.Error("failed to initialize recording", "error", err)
	}

	if ffmpegAvailable {
		slog.Info("starting encoder")
		if err := enc.Start(); err != nil {
			slog.Error("failed to start encoder", "error", err)
		}
	} else {
		slog.Warn("encoder not started - FFmpeg not available")
	}

	httpServer := srv.Start()

	return &app{
		cfg:        cfg,
		encoder:    enc,
		server:     srv,
		httpServer: httpServer,
	}, nil
}

// shutdown gracefully stops the HTTP server, encoder, and version checker.
func shutdown(a *app) {
	a.server.version.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	if err := a.encoder.Stop(); err != nil {
		slog.Error("error stopping encoder", "error", err)
	}
	if err := a.encoder.Close(); err != nil {
		slog.Error("error closing encoder resources", "error", err)
	}
}
