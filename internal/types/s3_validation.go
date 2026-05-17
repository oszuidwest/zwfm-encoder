package types

import (
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// S3ValidationCode identifies an S3 credential validation rule failure.
// Stored in validation.Issue.Code via string conversion; adapters cast
// back when switching on rule identity.
type S3ValidationCode string

const (
	// S3BucketRequired means the bucket name is empty.
	S3BucketRequired S3ValidationCode = "bucket_required"
	// S3AccessKeyIDRequired means the access key ID is empty.
	S3AccessKeyIDRequired S3ValidationCode = "access_key_id_required"
	// S3SecretAccessKeyRequired means the secret access key is empty.
	S3SecretAccessKeyRequired S3ValidationCode = "secret_access_key_required"
)

// ValidateS3Credentials reports issues for S3 credential presence: bucket,
// access key ID, and secret access key must all be non-empty. Callers
// attach the appropriate context message (e.g. recorder-conditional vs.
// test-handler) via their own adapter; this function does not own the
// presentation.
func ValidateS3Credentials(bucket, accessKeyID, secretAccessKey string) validation.Issues {
	var issues validation.Issues
	if bucket == "" {
		issues = append(issues, validation.Issue{Field: "s3_bucket", Code: string(S3BucketRequired)})
	}
	if accessKeyID == "" {
		issues = append(issues, validation.Issue{Field: "s3_access_key_id", Code: string(S3AccessKeyIDRequired)})
	}
	if secretAccessKey == "" {
		issues = append(issues, validation.Issue{Field: "s3_secret_access_key", Code: string(S3SecretAccessKeyRequired)})
	}
	return issues
}
