// Package output manages FFmpeg output processes for SRT streaming.
package output

import "time"

// OutputState represents the current state of an output.
type OutputState string

const (
	// StateStopped indicates the output is not running.
	StateStopped OutputState = "stopped"
	// StateStarting indicates FFmpeg is launched but not yet stable.
	StateStarting OutputState = "starting"
	// StateConnected indicates the output is running and stable (>=10 sec).
	StateConnected OutputState = "connected"
	// StateError indicates the output failed but retry continues.
	StateError OutputState = "error"
	// StateStopping indicates graceful shutdown in progress.
	StateStopping OutputState = "stopping"
	// StateGivenUp indicates max retries exceeded.
	StateGivenUp OutputState = "given_up"
)

// StartingTimeout is the maximum time to wait in starting state before transitioning to error.
const StartingTimeout = 30 * time.Second
