package recording

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// createS3Client creates an S3 client with the given configuration.
func createS3Client(cfg *S3Config) *s3.Client {
	options := s3.Options{
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Region:      "auto",
	}
	if cfg.Endpoint != "" {
		options.BaseEndpoint = aws.String(cfg.Endpoint)
		options.UsePathStyle = true
	}
	return s3.New(options)
}

// TestRecorderS3Connection tests S3 connectivity for a recorder configuration.
func TestRecorderS3Connection(recorder *types.Recorder) error {
	if !recorder.HasS3Credentials() {
		return fmt.Errorf("S3 is not configured")
	}

	cfg := RecorderToS3Config(recorder)
	client := createS3Client(cfg)

	ctx, cancel := context.WithTimeoutCause(
		context.Background(),
		30*time.Second,
		errors.New("s3 connection test timeout"),
	)
	defer cancel()

	testKey := fmt.Sprintf("test-connection-%d.txt", time.Now().UnixNano())
	testContent := []byte("ZuidWest FM encoder connection test")
	uploadID := rand.Text()

	// Recordings are uploaded with If-None-Match, so test that the store supports it.
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(cfg.Bucket),
		Key:           aws.String(testKey),
		Body:          bytes.NewReader(testContent),
		ContentLength: aws.Int64(int64(len(testContent))),
		IfNoneMatch:   aws.String("*"),
		Metadata:      map[string]string{"zwfm-upload-id": uploadID},
	})
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotImplemented" {
		return fmt.Errorf("storage does not support conditional writes (If-None-Match), which recordings need to never overwrite each other: %w", err)
	}
	// A committed PUT can lose its response and retry as 412, so read the
	// probe's identity either way: only our unique ID makes it ours to accept
	// and delete. The PUT may have used up the probe's deadline.
	headCtx, cancelHead := context.WithTimeout(context.Background(), 10*time.Second)
	head, headErr := client.HeadObject(headCtx, &s3.HeadObjectInput{
		Bucket: aws.String(cfg.Bucket), Key: aws.String(testKey),
	})
	cancelHead()
	owned := headErr == nil && head.Metadata["zwfm-upload-id"] == uploadID
	if err != nil && !owned {
		return fmt.Errorf("upload test file: %w", err)
	}
	defer func() {
		// The PUT may have used up the probe's deadline; clean up with a fresh one.
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		if _, err := client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{
			Bucket: aws.String(cfg.Bucket),
			Key:    aws.String(testKey),
		}); err != nil {
			slog.Warn("failed to delete test file", "key", testKey, "error", err)
		}
	}()
	if headErr != nil {
		return fmt.Errorf("read test object metadata (HeadObject permission is required for idempotent uploads): %w", headErr)
	}
	if !owned {
		return errors.New("storage does not preserve upload identity metadata, which recordings need to avoid duplicate uploads")
	}

	return nil
}
