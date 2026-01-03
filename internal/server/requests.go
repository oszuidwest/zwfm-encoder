package server

// Request types for WebSocket commands with validation tags.
// These types define the expected input for each command and use
// go-playground/validator struct tags for automatic validation.

// --- Audio settings ---

// AudioUpdateRequest is the request body for audio/update.
type AudioUpdateRequest struct {
	Input string `json:"input" validate:"omitempty"`
}

// --- Silence detection settings ---

// SilenceUpdateRequest is the request body for silence/update.
type SilenceUpdateRequest struct {
	ThresholdDB *float64 `json:"threshold_db" validate:"omitempty,gte=-60,lte=0"`
	DurationMs  *int64   `json:"duration_ms" validate:"omitempty,gte=500,lte=300000"`
	RecoveryMs  *int64   `json:"recovery_ms" validate:"omitempty,gte=500,lte=60000"`
}

// --- Silence dump settings ---

// SilenceDumpUpdateRequest is the request body for silence_dump/update.
type SilenceDumpUpdateRequest struct {
	Enabled       *bool `json:"enabled"`
	RetentionDays *int  `json:"retention_days" validate:"omitempty,gte=1,lte=365"`
}

// --- Notification settings ---

// WebhookUpdateRequest is the request body for notifications/webhook/update.
type WebhookUpdateRequest struct {
	URL string `json:"url" validate:"omitempty,max=2048"`
}

// LogUpdateRequest is the request body for notifications/log/update.
type LogUpdateRequest struct {
	Path string `json:"path" validate:"omitempty,max=4096"`
}

// EmailUpdateRequest is the request body for notifications/email/update.
type EmailUpdateRequest struct {
	TenantID     string `json:"tenant_id" validate:"omitempty,max=100"`
	ClientID     string `json:"client_id" validate:"omitempty,max=100"`
	ClientSecret string `json:"client_secret" validate:"omitempty,max=500"`
	FromAddress  string `json:"from_address" validate:"omitempty,max=254"`
	Recipients   string `json:"recipients" validate:"omitempty,max=1000"`
}

// ZabbixUpdateRequest is the request body for notifications/zabbix/update.
type ZabbixUpdateRequest struct {
	Server string `json:"server" validate:"omitempty,max=253"`
	Port   int    `json:"port" validate:"omitempty,gte=1,lte=65535"`
	Host   string `json:"host" validate:"omitempty,max=253"`
	Key    string `json:"key" validate:"omitempty,max=256"`
}

// --- Output entity ---

// OutputRequest is the request body for outputs/add and outputs/update.
type OutputRequest struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host" validate:"required,max=253,hostname|ip"`
	Port       int    `json:"port" validate:"required,gte=1,lte=65535"`
	Password   string `json:"password" validate:"omitempty,max=500"`
	StreamID   string `json:"stream_id" validate:"omitempty,max=256"`
	Codec      string `json:"codec" validate:"omitempty,oneof=wav mp3 mp2 ogg"`
	MaxRetries int    `json:"max_retries" validate:"omitempty,gte=0,lte=9999"`
}

// --- Recorder entity ---

// RecorderRequest is the request body for recorders/add and recorders/update.
type RecorderRequest struct {
	Name              string `json:"name" validate:"required,max=100"`
	Enabled           bool   `json:"enabled"`
	Codec             string `json:"codec" validate:"omitempty,oneof=wav mp3 mp2 ogg"`
	RotationMode      string `json:"rotation_mode" validate:"required,oneof=hourly ondemand"`
	StorageMode       string `json:"storage_mode" validate:"required,oneof=local s3 both"`
	LocalPath         string `json:"local_path" validate:"omitempty,max=4096"`
	S3Endpoint        string `json:"s3_endpoint" validate:"omitempty,max=2048"`
	S3Bucket          string `json:"s3_bucket" validate:"omitempty,max=63"`
	S3AccessKeyID     string `json:"s3_access_key_id" validate:"omitempty,max=128"`
	S3SecretAccessKey string `json:"s3_secret_access_key" validate:"omitempty,max=256"`
	RetentionDays     int    `json:"retention_days" validate:"omitempty,gte=1,lte=3650"`
}

// --- S3 test ---

// S3TestRequest is the request body for recorders/test-s3.
type S3TestRequest struct {
	Endpoint  string `json:"s3_endpoint" validate:"omitempty,max=2048"`
	Bucket    string `json:"s3_bucket" validate:"required,max=63"`
	AccessKey string `json:"s3_access_key_id" validate:"required,max=128"`
	SecretKey string `json:"s3_secret_access_key" validate:"required,max=256"`
}
