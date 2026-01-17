// Package main provides an audio streaming application that captures audio from digital input and streams to multiple SRT destinations.
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
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/encoder"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func main() {
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

	cfg := config.New(*configPath)
	if err := cfg.Load(); err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Check FFmpeg availability
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
		slog.Error("failed to create encoder", "error", err)
		os.Exit(1)
	}

	srv := NewServer(cfg, enc, ffmpegAvailable)

	// Initialize recording manager if configured
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

	// Start web server.
	httpServer := srv.Start()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, util.ShutdownSignals()...)
	<-sigChan

	slog.Info("shutting down")

	// Stop version checker goroutine
	srv.version.Stop()

	// Shut down HTTP server.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	if err := enc.Stop(); err != nil {
		slog.Error("error stopping encoder", "error", err)
	}

	slog.Info("shutdown complete")
}
