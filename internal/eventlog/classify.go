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

// Category returns the subsystem category for t.
func (t EventType) Category() Category {
	switch {
	case IsStreamEvent(t):
		return CategoryStream
	case IsSilenceEvent(t), IsChannelImbalanceEvent(t):
		return CategoryAudio
	case IsRecorderEvent(t):
		return CategoryRecorder
	default:
		return CategoryUnknown
	}
}

// Severity returns the display severity for t.
func (t EventType) Severity() Severity {
	switch t {
	case StreamError, RecorderError, UploadFailed, UploadAbandoned:
		return SeverityError
	case StreamRetry, SilenceStart, ChannelImbalanceStart, UploadRetry:
		return SeverityWarning
	case StreamStable, SilenceEnd, ChannelImbalanceEnd, UploadCompleted, CleanupCompleted:
		return SeveritySuccess
	case StreamStarted, StreamStopped, AudioDumpReady, RecorderStarted, RecorderStopped, RecorderFile, UploadQueued:
		return SeverityInfo
	default:
		return SeverityUnknown
	}
}

// Reason returns the semantic reason for t.
func (t EventType) Reason() Reason {
	switch t {
	case StreamError, StreamRetry, SilenceStart, ChannelImbalanceStart,
		RecorderError, UploadFailed, UploadRetry, UploadAbandoned:
		return ReasonProblem
	case StreamStable, SilenceEnd, ChannelImbalanceEnd:
		return ReasonRecovery
	case StreamStarted, StreamStopped, AudioDumpReady, RecorderStarted, RecorderStopped:
		return ReasonLifecycle
	case RecorderFile, UploadQueued, UploadCompleted, CleanupCompleted:
		return ReasonRoutine
	default:
		return ReasonUnknown
	}
}
