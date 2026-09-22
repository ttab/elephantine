package pacer

import "time"

// SetClock replaces the clock the failure budget is measured against, so that
// a test can exhaust a budget without waiting for it.
func (p *Pacer) SetClock(now func() time.Time) {
	p.now = now
}
