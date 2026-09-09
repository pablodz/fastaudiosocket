package fastaudiosocket

import (
	"context"
	"time"
)

const MaxCatchUpDrift = 500 * time.Millisecond

type pacer struct {
	interval time.Duration
	start    time.Time
	sent     int64
}

func newPacer(interval time.Duration) *pacer {
	return &pacer{interval: interval, start: time.Now()}
}

func (p *pacer) due() time.Time {
	return p.start.Add(time.Duration(p.sent) * p.interval)
}

func (p *pacer) advance() {
	p.sent++
}

func (p *pacer) resync(now time.Time) bool {
	if now.Sub(p.due()) <= MaxCatchUpDrift {
		return false
	}

	p.start = now.Add(-time.Duration(p.sent) * p.interval)

	return true
}

func (p *pacer) wait(playerCtx, callCtx context.Context) error {
	now := time.Now()
	p.resync(now)

	delay := p.due().Sub(now)
	if delay <= 0 {
		return pendingContextError(playerCtx, callCtx)
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-playerCtx.Done():
		return playerCtx.Err()
	case <-callCtx.Done():
		return callCtx.Err()
	case <-timer.C:
		return nil
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
