// Command micrompx-watchdog observes a local MicroMPX decoder without controlling it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"

	"github.com/oszuidwest/zwfm-encoder/internal/decodermonitor"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), util.ShutdownSignals()...)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}

func run(ctx context.Context, args []string, output, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("micrompx-watchdog", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	path := flags.String("config", "/etc/micrompx-watchdog/config.json", "Existing private watchdog JSON configuration")
	check := flags.Bool("check", false, "Validate configuration and exit; no polling, state writes or mail")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(diagnostics, "Usage: micrompx-watchdog [-config path] [-check]\n\nLocal Linux log and ALSA watchdog. Observe and notify only: never restarts,\nresets, changes decoder settings or opens a playback device. Sending email\nis opt-in via send_mail; the default mode logs notification previews.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(diagnostics, "unexpected positional arguments")
		return 2
	}
	cfg, err := decodermonitor.LoadConfig(*path)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, err)
		return 1
	}
	sender, recipients, err := decodermonitor.NewMailSender(&cfg)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, err)
		return 1
	}
	if *check {
		_, _ = fmt.Fprintln(output, "configuration valid; no mail sent or decoder accessed")
		return 0
	}
	if runtime.GOOS != "linux" {
		_, _ = fmt.Fprintln(diagnostics, "local decoder monitoring requires Linux")
		return 1
	}
	store, err := decodermonitor.OpenStore(&cfg)
	if err != nil {
		_, _ = fmt.Fprintln(diagnostics, err)
		return 1
	}
	defer func() {
		if err := store.Close(); err != nil {
			slog.Error("close watchdog state", "error", err)
		}
	}()
	runner := decodermonitor.Runner{Config: cfg, Store: store, Probe: decodermonitor.LocalProbe{PCMStatus: cfg.PCMStatus}, Sender: sender, Recipients: recipients}
	if err := runner.Run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0
		}
		slog.Error("watchdog stopped; decoder was not modified", "error", err)
		return 1
	}
	return 0
}
