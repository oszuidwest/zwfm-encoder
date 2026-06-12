package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestSetMaxDurationMinutesPropagatesToExistingRecorders(t *testing.T) {
	m, err := NewManager("", t.TempDir(), 60, nil)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &types.Recorder{
		ID:            "r1",
		Name:          "Test",
		Codec:         types.CodecPCM,
		RecordingMode: types.RecordingOnDemand,
		StorageMode:   types.StorageLocal,
		LocalPath:     t.TempDir(),
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}

	// Recorder was constructed with the initial 60-minute limit.
	if got := m.recorders["r1"].maxDurationMinutes; got != 60 {
		t.Fatalf("initial maxDurationMinutes: got %d, want 60", got)
	}

	m.SetMaxDurationMinutes(90)

	// The existing recorder must reflect the new limit without needing a restart.
	if got := m.recorders["r1"].maxDurationMinutes; got != 90 {
		t.Errorf("after SetMaxDurationMinutes(90): got %d, want 90", got)
	}
}

func TestRecorderCodecMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		codec           types.Codec
		wantExtension   string
		wantContentType string
	}{
		{name: "mp3", codec: types.CodecMP3, wantExtension: "mp3", wantContentType: "audio/mpeg"},
		{name: "opus", codec: types.CodecOpus, wantExtension: "ts", wantContentType: "audio/mp2t"},
		{name: "pcm", codec: types.CodecPCM, wantExtension: "ts", wantContentType: "audio/mp2t"},
		{name: "unknown falls back to mpeg ts", codec: types.Codec("aac"), wantExtension: "ts", wantContentType: "audio/mp2t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := &GenericRecorder{
				id:     "recorder-test",
				config: types.Recorder{Codec: tt.codec},
			}
			if got := recorder.getFileExtension(); got != tt.wantExtension {
				t.Fatalf("getFileExtension() = %q, want %q", got, tt.wantExtension)
			}
			if got := recorder.getContentType(); got != tt.wantContentType {
				t.Fatalf("getContentType() = %q, want %q", got, tt.wantContentType)
			}
		})
	}
}

func TestPendingUploadsPersistAndReload(t *testing.T) {
	t.Parallel()

	spoolDir := t.TempDir()
	cfg := testS3Recorder()

	m, err := NewManager("", spoolDir, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}

	recorder := m.recorders[cfg.ID]
	filePath := writeSpoolFile(t, spoolDir, cfg.ID, "one.mp3", "audio")
	req := uploadRequest{
		localPath:         filePath,
		s3Key:             "recordings/Test/one.mp3",
		fileSize:          5,
		deleteAfterUpload: true,
	}
	recorder.addToRetryQueue(req, "s3 down")

	if got := len(recorder.retryQueue); got != 1 {
		t.Fatalf("retryQueue len = %d, want 1", got)
	}
	metadataPath := recorder.retryQueue[0].metadataPath
	if _, err := os.Stat(metadataPath); err != nil {
		t.Fatalf("stat metadata: %v", err)
	}

	reloaded, err := NewManager("", spoolDir, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}

	got := reloaded.recorders[cfg.ID].retryQueue
	if len(got) != 1 {
		t.Fatalf("reloaded retryQueue len = %d, want 1", len(got))
	}
	if got[0].request.localPath != req.localPath {
		t.Fatalf("localPath = %q, want %q", got[0].request.localPath, req.localPath)
	}
	if got[0].request.s3Key != req.s3Key {
		t.Fatalf("s3Key = %q, want %q", got[0].request.s3Key, req.s3Key)
	}
	if !got[0].request.deleteAfterUpload {
		t.Fatal("deleteAfterUpload = false, want true")
	}
	if got[0].metadataPath != metadataPath {
		t.Fatalf("metadataPath = %q, want %q", got[0].metadataPath, metadataPath)
	}
}

func TestReconcilePendingUploadsRecoversS3OnlySpoolFilesWithoutMetadata(t *testing.T) {
	t.Parallel()

	spoolDir := t.TempDir()
	cfg := testS3Recorder()
	filePath := writeSpoolFile(t, spoolDir, cfg.ID, "orphan.mp3", "audio")

	m, err := NewManager("", spoolDir, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}

	got := m.recorders[cfg.ID].retryQueue
	if len(got) != 1 {
		t.Fatalf("retryQueue len = %d, want 1", len(got))
	}
	if got[0].request.localPath != filePath {
		t.Fatalf("localPath = %q, want %q", got[0].request.localPath, filePath)
	}
	if got[0].request.s3Key != "recordings/Test/orphan.mp3" {
		t.Fatalf("s3Key = %q, want recovered object key", got[0].request.s3Key)
	}
	if _, err := os.Stat(got[0].metadataPath); err != nil {
		t.Fatalf("stat recovered metadata: %v", err)
	}
}

func TestProcessRetryQueueAbandonsExpiredUploadAndRemovesMetadata(t *testing.T) {
	t.Parallel()

	spoolDir := t.TempDir()
	cfg := testS3Recorder()
	recorder, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg,
		SpoolDir: spoolDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	filePath := writeSpoolFile(t, spoolDir, cfg.ID, "expired.mp3", "audio")
	pending := pendingUpload{
		request: uploadRequest{
			localPath:         filePath,
			s3Key:             "recordings/Test/expired.mp3",
			fileSize:          5,
			deleteAfterUpload: true,
		},
		firstAttempt: time.Now().Add(-MaxUploadRetryAge - time.Hour),
		retryCount:   3,
		lastError:    "still down",
	}
	pending.metadataPath = recorder.pendingUploadMetadataPath(pending.request)
	if err := recorder.savePendingUpload(&pending); err != nil {
		t.Fatal(err)
	}
	recorder.retryQueue = []pendingUpload{pending}

	recorder.processRetryQueue()

	if got := len(recorder.retryQueue); got != 0 {
		t.Fatalf("retryQueue len = %d, want 0", got)
	}
	if _, err := os.Stat(pending.metadataPath); !os.IsNotExist(err) {
		t.Fatalf("metadata stat error = %v, want not exist", err)
	}
}

func TestProcessRetryQueueRemovesMetadataForMissingFile(t *testing.T) {
	t.Parallel()

	spoolDir := t.TempDir()
	cfg := testS3Recorder()
	recorder, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg,
		SpoolDir: spoolDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	pending := pendingUpload{
		request: uploadRequest{
			localPath: filepath.Join(spoolDir, "missing.mp3"),
			s3Key:     "recordings/Test/missing.mp3",
			fileSize:  5,
		},
		firstAttempt: time.Now(),
		lastError:    "missing",
	}
	pending.metadataPath = recorder.pendingUploadMetadataPath(pending.request)
	if err := recorder.savePendingUpload(&pending); err != nil {
		t.Fatal(err)
	}
	recorder.retryQueue = []pendingUpload{pending}

	recorder.processRetryQueue()

	if got := len(recorder.retryQueue); got != 0 {
		t.Fatalf("retryQueue len = %d, want 0", got)
	}
	if _, err := os.Stat(pending.metadataPath); !os.IsNotExist(err) {
		t.Fatalf("metadata stat error = %v, want not exist", err)
	}
}

func TestStopPreservesRetryQueue(t *testing.T) {
	t.Parallel()

	cfg := testS3Recorder()
	recorder, err := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg,
		SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder.state = types.ProcessRunning
	recorder.retryQueue = []pendingUpload{
		{
			request: uploadRequest{
				localPath: "one.mp3",
				s3Key:     "recordings/Test/one.mp3",
			},
			firstAttempt: time.Now(),
		},
	}

	if err := recorder.Stop(); err != nil {
		t.Fatal(err)
	}

	if got := len(recorder.retryQueue); got != 1 {
		t.Fatalf("retryQueue len after Stop = %d, want 1", got)
	}
}

func testS3Recorder() *types.Recorder {
	return &types.Recorder{
		ID:                "r1",
		Name:              "Test",
		Codec:             types.CodecMP3,
		RecordingMode:     types.RecordingHourly,
		StorageMode:       types.StorageS3,
		S3Bucket:          "bucket",
		S3AccessKeyID:     "access",
		S3SecretAccessKey: "secret",
	}
}

func writeSpoolFile(t *testing.T, spoolDir, recorderID, name, data string) string {
	t.Helper()

	path := filepath.Join(spoolDir, "recorders", recorderID, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir spool: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write spool file: %v", err)
	}
	return path
}
