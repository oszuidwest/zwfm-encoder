package encoder

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// TestStartRejectsStateStopping verifies that Start() returns ErrAlreadyRunning when the
// encoder is in StateStopping. This guards against the regression where the MaxRetries
// exit path set state = StateStopped before cleanup finished, allowing a concurrent
// Start() to race the in-progress cleanup (StopAll, silenceDumpManager.Stop, DrainLogs).
// The fix transitions through StateStopping during cleanup; Start() must block that state.
func TestStartRejectsStateStopping(t *testing.T) {
	cfg := config.New(filepath.Join(t.TempDir(), "config.json"))
	// Set a non-empty AudioInput so Start() reaches the state guard rather than
	// returning ErrNoAudioInput before it gets there. All required silence fields
	// must be valid because ApplySettings now validates before applying.
	if err := cfg.ApplySettings(&config.SettingsUpdate{
		AudioInput:        "test-device",
		SilenceThreshold:  config.DefaultSilenceThreshold,
		SilenceDurationMs: config.DefaultSilenceDurationMs,
		SilenceRecoveryMs: config.DefaultSilenceRecoveryMs,
	}); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	e := &Encoder{
		config: cfg,
		state:  types.StateStopping,
	}

	if err := e.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("Start() in StateStopping = %v, want ErrAlreadyRunning", err)
	}
}
