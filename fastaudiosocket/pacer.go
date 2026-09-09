package fastaudiosocket

import (
	"context"
	"time"
)

const MaxCatchUpDrift = 500 * time.Millisecond

type pacer struct {
	interval        time.Duration
	start           time.Time
	sent            int64
	lead            time.Duration
	timer           *time.Timer
	resyncs         uint64
	floor           time.Time
	maxLate         time.Duration
	maxScheduleLate time.Duration
}

func newPacer(interval time.Duration) *pacer {
	now := time.Now()
	return &pacer{interval: interval, start: now, floor: now}
}

func (p *pacer) due() time.Time {
	return p.mediaDue().Add(-p.advanceBy())
}

func (p *pacer) mediaDue() time.Time {
	return p.start.Add(time.Duration(p.sent) * p.interval)
}

func (p *pacer) advanceBy() time.Duration {
	// Include the current frame: a 100 ms target prefills five frames, not six.
	return max(0, p.lead-p.interval)
}

func (p *pacer) advance() {
	p.sent++
}

func (p *pacer) advanceAt(now time.Time) {
	if p.lead > 0 && now.After(p.mediaDue()) {
		// After an estimated underrun, refill only the configured lead.
		p.start = now.Add(-time.Duration(p.sent) * p.interval)
		p.floor = now
	}
	p.advance()
}

func (p *pacer) resync(now time.Time) bool {
	p.maxLate = max(p.maxLate, now.Sub(p.mediaDue()))
	due := p.due()
	if due.Before(p.floor) {
		due = p.floor
	}
	p.maxScheduleLate = max(p.maxScheduleLate, now.Sub(due))
	if now.Sub(p.mediaDue()) <= MaxCatchUpDrift {
		return false
	}

	p.start = now.Add(-time.Duration(p.sent) * p.interval)
	p.floor = now
	p.resyncs++

	return true
}

func (p *pacer) wait(playerCtx, callCtx context.Context) error {
	if err := pendingContextError(playerCtx, callCtx); err != nil {
		return err
	}
	now := time.Now()
	if p.lead > 0 && p.sent == 0 && now.After(p.start) {
		p.start = now // Start when audio is available, not when a stream opens.
		p.floor = now
	}
	p.resync(now)

	delay := p.due().Sub(now)
	if delay <= 0 {
		return pendingContextError(playerCtx, callCtx)
	}

	if p.timer == nil {
		p.timer = time.NewTimer(delay)
	} else {
		p.timer.Reset(delay)
	}

	select {
	case <-playerCtx.Done():
		return playerCtx.Err()
	case <-callCtx.Done():
		return callCtx.Err()
	case <-p.timer.C:
		// The scheduler can also stall while the timer is pending.
		p.resync(time.Now())
		return pendingContextError(playerCtx, callCtx)
	}
}

func (p *pacer) stop() {
	if p.timer != nil {
		p.timer.Stop()
	}
}

func pendingContextError(playerCtx, callCtx context.Context) error {
	select {
	case <-playerCtx.Done():
		return playerCtx.Err()
	case <-callCtx.Done():
		return callCtx.Err()
	default:
		return nil
	}
}
