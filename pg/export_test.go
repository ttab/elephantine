package pg

import "time"

// Exported for testing.
type RestartPacer = restartPacer

// Exported for testing.
const (
	RestartMinRuntime     time.Duration = restartMinRuntime
	RestartBackoffFloor   time.Duration = restartBackoffFloor
	RestartBackoffCeil    time.Duration = restartBackoffCeil
	DefaultHealthyRuntime time.Duration = defaultHealthyRuntime
)

// NewRestartPacer exposes the restart pacer for testing.
func NewRestartPacer(healthyRuntime time.Duration, maxFailures int) *RestartPacer {
	return &restartPacer{
		healthyRuntime: healthyRuntime,
		maxFailures:    maxFailures,
	}
}
