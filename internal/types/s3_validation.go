package types

import (
	"github.com/oszuidwest/zwfm-encoder/internal/validation"
)

// S3 validation rule identifiers. Stored in validation.Issue.Code; callers
// switch on these to map rules to context-specific messages.
const (
	// S3BucketRequired means the bucket name is empty.
	S3BucketRequired = "bucket_required"
	// S3AccessKeyIDRequired means the access key ID is empty.
	S3AccessKeyIDRequired = "access_key_id_required"
	// S3SecretAccessKeyRequired means the secret access key is empty.
	S3SecretAccessKeyRequired = "secret_access_key_required"
)

// ValidateS3Credentials reports issues for S3 credential presence: bucket,
// access key ID, and secret access key must all be non-empty. Callers
// attach the appropriate context message (e.g. recorder-conditional vs.
// test-handler) via their own adapter; this function does not own the
// presentation.
func ValidateS3Credentials(bucket, accessKeyID, secretAccessKey string) validation.Issues {
	var issues validation.Issues
	if bucket == "" {
		issues = append(issues, validation.Issue{Field: "s3_bucket", Code: S3BucketRequired})
	}
	if accessKeyID == "" {
		issues = append(issues, validation.Issue{Field: "s3_access_key_id", Code: S3AccessKeyIDRequired})
	}
	if secretAccessKey == "" {
		issues = append(issues, validation.Issue{Field: "s3_secret_access_key", Code: S3SecretAccessKeyRequired})
	}
	return issues
}
