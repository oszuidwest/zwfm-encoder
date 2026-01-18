// Package recording provides audio recording with S3 upload capabilities.
package recording

import (
	"errors"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// Sentinel errors for recording operations.
var (
	// ErrHourlyRecorderNotControllable is returned when trying to start/stop an hourly recorder via API.
	ErrHourlyRecorderNotControllable = errors.New("hourly recorders cannot be started/stopped via API")

	// ErrAlreadyRecording is returned when trying to start a recorder that is already recording.
	ErrAlreadyRecording = errors.New("recorder is already recording")

	// ErrNotRecording is returned when trying to stop a recorder that is not recording.
	ErrNotRecording = errors.New("recorder is not recording")
)

// S3Config is the configuration for S3-compatible storage.
type S3Config struct {
	// Endpoint is the custom S3 endpoint (empty for AWS).
	Endpoint string `json:"endpoint,omitempty"`
	// Bucket is the S3 bucket name.
	Bucket string `json:"bucket,omitempty"`
	// AccessKeyID is the AWS access key ID.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// SecretAccessKey is the AWS secret access key.
	SecretAccessKey string `json:"secret_access_key,omitempty"`
}

// IsConfigured reports whether S3 settings are configured.
func (c *S3Config) IsConfigured() bool {
	return c.Bucket != "" && c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// DefaultTempDir is the default temporary directory for recordings.
const DefaultTempDir = "/tmp/encoder-recordings"

// RecorderToS3Config extracts S3 configuration from a Recorder.
func RecorderToS3Config(r *types.Recorder) *S3Config {
	return &S3Config{
		Endpoint:        r.S3Endpoint,
		Bucket:          r.S3Bucket,
		AccessKeyID:     r.S3AccessKeyID,
		SecretAccessKey: r.S3SecretAccessKey,
	}
}
