package types

import (
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
	"slices"
	"testing"
)

func TestValidateS3Credentials(t *testing.T) {
	tests := []struct {
		name                              string
		bucket, accessKeyID, secretAccess string
		wantCodes                         []string
	}{
		{
			name:   "all fields set: no issues",
			bucket: "my-bucket", accessKeyID: "AKIA", secretAccess: "shh",
			wantCodes: nil,
		},
		{
			name:      "all empty: three issues in field order",
			wantCodes: []string{S3BucketRequired, S3AccessKeyIDRequired, S3SecretAccessKeyRequired},
		},
		{
			name:        "missing bucket only",
			accessKeyID: "AKIA", secretAccess: "shh",
			wantCodes: []string{S3BucketRequired},
		},
		{
			name:   "missing access key only",
			bucket: "my-bucket", secretAccess: "shh",
			wantCodes: []string{S3AccessKeyIDRequired},
		},
		{
			name:   "missing secret only",
			bucket: "my-bucket", accessKeyID: "AKIA",
			wantCodes: []string{S3SecretAccessKeyRequired},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s3Codes(ValidateS3Credentials(tt.bucket, tt.accessKeyID, tt.secretAccess))
			if !slices.Equal(got, tt.wantCodes) {
				t.Fatalf("ValidateS3Credentials codes = %v, want %v", got, tt.wantCodes)
			}
		})
	}
}
func s3Codes(issues validation.Issues) []string {
	if len(issues) == 0 {
		return nil
	}
	codes := make([]string, 0, len(issues))
	for _, issue := range issues {
		codes = append(codes, issue.Code)
	}
	return codes
}

func TestRecorderValidateS3Conditional(t *testing.T) {
	tests := []struct {
		name    string
		mode    StorageMode
		wantErr string
	}{
		{
			name:    "local mode: no S3 checks",
			mode:    StorageLocal,
			wantErr: "",
		},
		{
			name:    "s3 mode: bucket required",
			mode:    StorageS3,
			wantErr: "s3_bucket: is required for s3/both storage mode",
		},
		{
			name:    "both mode: bucket required",
			mode:    StorageBoth,
			wantErr: "s3_bucket: is required for s3/both storage mode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Recorder{
				Name:          "rec",
				Codec:         CodecPCM,
				RecordingMode: RecordingHourly,
				StorageMode:   tt.mode,
				LocalPath:     "/tmp/rec", // Satisfies local/both mode.
			}
			err := r.Validate()
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if gotErr != tt.wantErr {
				t.Fatalf("Validate() error = %q, want %q", gotErr, tt.wantErr)
			}
		})
	}
}

func TestRecorderValidateS3FirstErrorOrder(t *testing.T) {
	cases := []struct {
		name    string
		r       *Recorder
		wantErr string
	}{
		{
			name: "all S3 empty -> bucket reported first",
			r: &Recorder{
				Name: "rec", Codec: CodecPCM, RecordingMode: RecordingHourly, StorageMode: StorageS3,
			},
			wantErr: "s3_bucket: is required for s3/both storage mode",
		},
		{
			name: "bucket set, rest empty -> access key reported",
			r: &Recorder{
				Name: "rec", Codec: CodecPCM, RecordingMode: RecordingHourly, StorageMode: StorageS3,
				S3Bucket: "b",
			},
			wantErr: "s3_access_key_id: is required for s3/both storage mode",
		},
		{
			name: "bucket+access set, secret empty -> secret reported",
			r: &Recorder{
				Name: "rec", Codec: CodecPCM, RecordingMode: RecordingHourly, StorageMode: StorageS3,
				S3Bucket: "b", S3AccessKeyID: "k",
			},
			wantErr: "s3_secret_access_key: is required for s3/both storage mode",
		},
		{
			name: "all set -> no S3 error (bitrate PCM still 0 default, no error)",
			r: &Recorder{
				Name: "rec", Codec: CodecPCM, RecordingMode: RecordingHourly, StorageMode: StorageS3,
				S3Bucket: "b", S3AccessKeyID: "k", S3SecretAccessKey: "s",
			},
			wantErr: "",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.r.Validate()
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if gotErr != tt.wantErr {
				t.Fatalf("Validate() error = %q, want %q", gotErr, tt.wantErr)
			}
		})
	}
}
