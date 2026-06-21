package eventlog

// Severity classifies how urgent an event type is for API consumers.
type Severity string

const (
	// SeverityError indicates a failure that needs operator attention.
	SeverityError Severity = "error"
	// SeverityWarning indicates degraded or retrying behavior.
	SeverityWarning Severity = "warning"
	// SeveritySuccess indicates a recovery or successful completion.
	SeveritySuccess Severity = "success"
	// SeverityInfo indicates neutral lifecycle or routine information.
	SeverityInfo Severity = "info"
	// SeverityUnknown indicates an unrecognized event type.
	SeverityUnknown Severity = "unknown"
)

// Reason classifies why an event type matters to the events UI.
type Reason string

const (
	// ReasonProblem indicates an event type that represents a problem.
	ReasonProblem Reason = "problem"
	// ReasonRecovery indicates an event type that recovers from a problem.
	ReasonRecovery Reason = "recovery"
	// ReasonLifecycle indicates an intentional start, stop, or state transition.
	ReasonLifecycle Reason = "lifecycle"
	// ReasonRoutine indicates high-volume healthy recorder activity.
	ReasonRoutine Reason = "routine"
	// ReasonUnknown indicates an unrecognized event type.
	ReasonUnknown Reason = "unknown"
)

// Category classifies the subsystem that emitted an event type.
type Category string

const (
	// CategoryStream indicates stream output events.
	CategoryStream Category = "stream"
	// CategoryAudio indicates input audio health events.
	CategoryAudio Category = "audio"
	// CategoryRecorder indicates recorder and upload events.
	CategoryRecorder Category = "recorder"
	// CategoryUnknown indicates an unrecognized event type.
	CategoryUnknown Category = "unknown"
)

type eventClassification struct {
	category Category
	severity Severity
	reason   Reason
}

var allEventTypes = [...]EventType{
	StreamStarted,
	StreamStable,
	StreamError,
	StreamRetry,
	StreamStopped,
	SilenceStart,
	SilenceEnd,
	AudioDumpReady,
	ChannelImbalanceStart,
	ChannelImbalanceEnd,
	RecorderStarted,
	RecorderStopped,
	RecorderError,
	RecorderFile,
	UploadQueued,
	UploadCompleted,
	UploadFailed,
	UploadRetry,
	UploadAbandoned,
	CleanupCompleted,
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

// Category returns the subsystem category for t.
func (t EventType) Category() Category {
	if classification, ok := eventClassifications[t]; ok {
		return classification.category
	}
	return CategoryUnknown
}

// Severity returns the display severity for t.
func (t EventType) Severity() Severity {
	if classification, ok := eventClassifications[t]; ok {
		return classification.severity
	}
	return SeverityUnknown
}

// Reason returns the semantic reason for t.
func (t EventType) Reason() Reason {
	if classification, ok := eventClassifications[t]; ok {
		return classification.reason
	}
	return ReasonUnknown
}
