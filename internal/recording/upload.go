package recording

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// createS3Client creates an S3 client with the given configuration.
func createS3Client(cfg *S3Config) (*s3.Client, error) {
	creds := credentials.NewStaticCredentialsProvider(
		cfg.AccessKeyID,
		cfg.SecretAccessKey,
		"",
	)

	options := []func(*s3.Options){
		func(o *s3.Options) {
			o.Credentials = creds
			o.Region = "auto"
		},
	}

	if cfg.Endpoint != "" {
		options = append(options, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		})
	}

	return s3.New(s3.Options{}, options...), nil
}

// TestS3Connection tests connectivity to an S3 bucket by uploading and deleting a test file.
func TestS3Connection(cfg *S3Config) error {
	if !cfg.IsConfigured() {
		return fmt.Errorf("S3 is not configured")
	}

	client, err := createS3Client(cfg)
	if err != nil {
		return fmt.Errorf("create S3 client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30000*time.Millisecond)
	defer cancel()

	testKey := fmt.Sprintf("test-connection-%d.txt", time.Now().UnixNano())
	testContent := []byte("ZuidWest FM encoder connection test")

	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(cfg.Bucket),
		Key:           aws.String(testKey),
		Body:          bytes.NewReader(testContent),
		ContentLength: aws.Int64(int64(len(testContent))),
	})
	if err != nil {
		return fmt.Errorf("upload test file: %w", err)
	}

	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(testKey),
	})
	if err != nil {
		slog.Warn("failed to delete test file", "key", testKey, "error", err)
	}

	return nil
}
