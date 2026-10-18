package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/oszuidwest/zwfm-encoder/internal/config"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func recordingStorageMessage(paths []types.RecordingStorageStatus) (event, message string) {
	event = "recording_storage_restored"
	var lines []string
	for _, p := range paths {
		if p.Low || p.Error != "" {
			event = "recording_storage_low"
		}
		line := fmt.Sprintf("%s: %d bytes available; warning below %d bytes", p.Path, p.AvailableBytes, p.ThresholdBytes)
		if p.Error != "" {
			line += "; check failed: " + p.Error
		}
		lines = append(lines, line)
	}
	return event, strings.Join(lines, "\n")
}

// SendRecordingStorage posts the complete path state, including zero free bytes.
func (c *WebhookChannel) SendRecordingStorage(
	ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus,
) error {
	event, message := recordingStorageMessage(paths)
	return sendWebhook(ctx, cfg.WebhookURL, &WebhookPayload{
		Event: event, Message: message, Timestamp: timestampUTC(), StoragePaths: paths,
	})
}

func recordingStorageEmail(station string, paths []types.RecordingStorageStatus) (subject, body string) {
	event, message := recordingStorageMessage(paths)
	subject = "[OK] Recording Storage Restored - " + station
	if event == "recording_storage_low" {
		subject = "[ALERT] Recording Storage Low or Unavailable - " + station
	}
	return subject, message + "\n\nNo recordings have been automatically evicted. Check disk space and S3 uploads."
}

// SendRecordingStorage emails a warning or aggregate recovery using the existing mail configuration.
func (c *EmailChannel) SendRecordingStorage(
	ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus,
) error {
	graphCfg := BuildGraphConfig(cfg)
	client, err := NewGraphClient(graphCfg)
	if err != nil {
		return err
	}
	subject, body := recordingStorageEmail(cfg.StationName, paths)
	return sendEmailWithClient(ctx, graphCfg, client, subject, body)
}

// SendRecordingStorage uses a separate item derived from the upload key so disk
// recoveries cannot clear upload-abandonment alerts. Only aggregate state is sent:
// recovery of one path must not hide another path that is still low.
func (c *ZabbixChannel) SendRecordingStorage(
	ctx context.Context, cfg *config.Snapshot, paths []types.RecordingStorageStatus,
) error {
	if !c.IsConfiguredForUpload(cfg) {
		return nil
	}
	event, _ := recordingStorageMessage(paths)
	value := "event=RECORDING_STORAGE_RESTORED"
	if event == "recording_storage_low" {
		value = "event=RECORDING_STORAGE_LOW"
	}
	return sendZabbixEvent(ctx, cfg.ZabbixServer, cfg.ZabbixPort, cfg.ZabbixHost, cfg.ZabbixUploadKey+".storage", value)
}
