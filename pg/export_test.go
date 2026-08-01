package pg

import "time"

// Exported for testing.
type RestartPacer = restartPacer

// Exported for testing.
const (
	RestartMinRuntime   time.Duration = restartMinRuntime
	RestartBackoffFloor time.Duration = restartBackoffFloor
	RestartBackoffCeil  time.Duration = restartBackoffCeil
)
