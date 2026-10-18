package recording

import (
	"path/filepath"
	"testing"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestProtoRemovedLowPathClosesIncident(t *testing.T) {
	m := storageTestManager(t, t.TempDir())
	cfg := localOnDemandRecorder(t, "local")
	cfg.Enabled = true
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	m.eventLogger = logger
	var last []types.RecordingStorageStatus
	m.SetStorageChangeCallback(func(p []types.RecordingStorageStatus) { last = p })
	m.diskSpace = func(p string) (uint64, uint64, error) {
		if p == filepath.Clean(cfg.LocalPath) {
			return 0, 10 << 30, nil
		}
		return 10 << 30, 20 << 30, nil
	}
	m.checkStorage(m.cleanupStopCh)
	if err := m.RemoveRecorder(cfg.ID); err != nil {
		t.Fatal(err)
	}
	m.checkStorage(m.cleanupStopCh)
	events, _, _ := eventlog.ReadLast(logPath, 100, 0, eventlog.FilterRecorder)
	var low, restored int
	for _, e := range events {
		switch e.Type {
		case eventlog.RecordingStorageLow:
			low++
		case eventlog.RecordingStorageRestored:
			restored++
		}
	}
	t.Logf("low=%d restored=%d aggregate callback paths=%d", low, restored, len(last))
	if restored != 1 {
		t.Fatalf("removed low path never closed its incident: low=%d restored=%d", low, restored)
	}
}
