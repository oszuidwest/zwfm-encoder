package eventlog

// Severity classifies event urgency for API consumers.
type Severity string

const (
	// SeverityError marks a failure that needs operator attention.
	SeverityError Severity = "error"
	// SeverityWarning marks degraded or retrying behavior.
	SeverityWarning Severity = "warning"
	// SeveritySuccess marks recovery or successful completion.
	SeveritySuccess Severity = "success"
	// SeverityInfo marks neutral lifecycle or routine activity.
	SeverityInfo Severity = "info"
	// SeverityUnknown marks an unclassified event type.
	SeverityUnknown Severity = "unknown"
)

// Reason classifies why an event is grouped.
type Reason string

const (
	// ReasonProblem marks an active or failed problem.
	ReasonProblem Reason = "problem"
	// ReasonRecovery marks recovery from a problem.
	ReasonRecovery Reason = "recovery"
	// ReasonLifecycle marks an intentional state change.
	ReasonLifecycle Reason = "lifecycle"
	// ReasonRoutine marks high-volume healthy recorder activity.
	ReasonRoutine Reason = "routine"
	// ReasonUnknown marks an unclassified event type.
	ReasonUnknown Reason = "unknown"
)

// Category classifies event source subsystems.
type Category string

const (
	// CategoryStream marks stream output events.
	CategoryStream Category = "stream"
	// CategoryAudio marks input audio health events.
	CategoryAudio Category = "audio"
	// CategoryRecorder marks recorder and upload events.
	CategoryRecorder Category = "recorder"
	// CategoryUnknown marks an unclassified event type.
	CategoryUnknown Category = "unknown"
)

type eventClassification struct {
	category Category
	severity Severity
	reason   Reason
}

var eventClassifications = map[EventType]eventClassification{
	StreamStarted:         {category: CategoryStream, severity: SeverityInfo, reason: ReasonLifecycle},
	StreamStable:          {category: CategoryStream, severity: SeveritySuccess, reason: ReasonRecovery},
	StreamError:           {category: CategoryStream, severity: SeverityError, reason: ReasonProblem},
	StreamRetry:           {category: CategoryStream, severity: SeverityWarning, reason: ReasonProblem},
	StreamStopped:         {category: CategoryStream, severity: SeverityInfo, reason: ReasonLifecycle},
	SilenceStart:          {category: CategoryAudio, severity: SeverityWarning, reason: ReasonProblem},
	SilenceEnd:            {category: CategoryAudio, severity: SeveritySuccess, reason: ReasonRecovery},
	AudioDumpReady:        {category: CategoryAudio, severity: SeverityInfo, reason: ReasonLifecycle},
	ChannelImbalanceStart: {category: CategoryAudio, severity: SeverityWarning, reason: ReasonProblem},
	ChannelImbalanceEnd:   {category: CategoryAudio, severity: SeveritySuccess, reason: ReasonRecovery},
	RecorderStarted:       {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonLifecycle},
	RecorderStopped:       {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonLifecycle},
	RecorderError:         {category: CategoryRecorder, severity: SeverityError, reason: ReasonProblem},
	RecorderFile:          {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonRoutine},
	UploadQueued:          {category: CategoryRecorder, severity: SeverityInfo, reason: ReasonRoutine},
	UploadCompleted:       {category: CategoryRecorder, severity: SeveritySuccess, reason: ReasonRoutine},
	UploadFailed:          {category: CategoryRecorder, severity: SeverityError, reason: ReasonProblem},
	UploadRetry:           {category: CategoryRecorder, severity: SeverityWarning, reason: ReasonProblem},
	UploadAbandoned:       {category: CategoryRecorder, severity: SeverityError, reason: ReasonProblem},
	CleanupCompleted:      {category: CategoryRecorder, severity: SeveritySuccess, reason: ReasonRoutine},
}

// Category returns t's subsystem category.
func (t EventType) Category() Category {
	if classification, ok := eventClassifications[t]; ok {
		return classification.category
	}
	return CategoryUnknown
}

// Severity returns t's display severity.
func (t EventType) Severity() Severity {
	if classification, ok := eventClassifications[t]; ok {
		return classification.severity
	}
	return SeverityUnknown
}

// Reason returns t's event grouping reason.
func (t EventType) Reason() Reason {
	if classification, ok := eventClassifications[t]; ok {
		return classification.reason
	}
	return ReasonUnknown
}
