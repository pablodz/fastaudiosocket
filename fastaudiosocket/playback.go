package fastaudiosocket

import (
	"context"
	"errors"
	"time"
)

var ErrPlaybackInProgress = errors.New("playback already in progress")

// PlaybackOptions applies to all Play methods on a socket.
type PlaybackOptions struct {
	// Lead targets audio handed to the socket ahead of estimated playout,
	// including the current frame. Zero preserves legacy deadline pacing.
	// Positive values must be multiples of TickerInterval. The peer must
	// buffer early audio; cancellation cannot retract it.
	Lead time.Duration
}

// PlaybackStats is a cumulative snapshot for this socket. AudioSocket has no
// playout acknowledgements: EstimatedBuffered assumes immediate delivery and
// consumption at 8 kHz. It is not confirmed audible playback or an upper bound
// on network/receiver buffering.
type PlaybackStats struct {
	PacketsSent       uint64
	AudioSent         time.Duration
	EstimatedBuffered time.Duration
	LatePackets       uint64
	MaxLateness       time.Duration
	// MaxScheduleLateness includes lateness covered by Lead. It measures
	// successful write completion against dispatch deadlines, excluding prefill.
	MaxScheduleLateness time.Duration
	Resyncs             uint64
	WriteErrors         uint64
}

// SetPlaybackOptions configures subsequent playback calls. It is safe during
// playback; the active call keeps its existing options.
func (s *FastAudioSocket) SetPlaybackOptions(opts PlaybackOptions) error {
	if opts.Lead < 0 || opts.Lead%TickerInterval != 0 {
		return errors.New("playback lead must be a nonnegative multiple of 20ms")
	}
	s.playbackStateMu.Lock()
	s.playbackOptions = opts
	s.playbackStateMu.Unlock()
	return nil
}

func (s *FastAudioSocket) PlaybackStats() PlaybackStats {
	s.playbackStateMu.Lock()
	defer s.playbackStateMu.Unlock()
	stats := s.playbackStats
	stats.EstimatedBuffered = max(0, time.Until(s.playoutEnd))
	return stats
}

func (s *FastAudioSocket) beginPlayback() (*pacer, error) {
	if !s.playbackMu.TryLock() {
		return nil, ErrPlaybackInProgress
	}
	s.playbackStateMu.Lock()
	defer s.playbackStateMu.Unlock()
	p := newPacer(TickerInterval)
	p.lead = s.playbackOptions.Lead
	if p.lead > 0 && s.playoutEnd.After(p.start) {
		// Short sequential calls cannot each add a new lead to the queue.
		p.start = s.playoutEnd
	}
	return p, nil
}

func (s *FastAudioSocket) finishPlayback(p *pacer) {
	p.stop()
	s.playbackMu.Unlock()
}

func (s *FastAudioSocket) recordWrite(p *pacer, now time.Time, err error) {
	s.playbackStateMu.Lock()
	defer s.playbackStateMu.Unlock()
	s.playbackStats.Resyncs += p.resyncs
	s.playbackStats.MaxLateness = max(s.playbackStats.MaxLateness, p.maxLate)
	s.playbackStats.MaxScheduleLateness = max(s.playbackStats.MaxScheduleLateness, p.maxScheduleLate)
	p.resyncs = 0
	if err != nil {
		s.playbackStats.WriteErrors++
		return
	}
	due := p.due()
	if due.Before(p.floor) {
		due = p.floor
	}
	s.playbackStats.MaxScheduleLateness = max(s.playbackStats.MaxScheduleLateness, now.Sub(due))
	// Compare against media deadlines; intentional prefill is not lateness.
	if lateness := now.Sub(p.mediaDue()); lateness > 0 {
		s.playbackStats.LatePackets++
		s.playbackStats.MaxLateness = max(s.playbackStats.MaxLateness, lateness)
	}
	if now.After(s.playoutEnd) {
		s.playoutEnd = now
	}
	s.playoutEnd = s.playoutEnd.Add(TickerInterval)
	s.playbackStats.PacketsSent++
	s.playbackStats.AudioSent += TickerInterval
	p.advanceAt(now)
}

// playbackContext cancels control waits and blocked writes on either context.
// The connection is owned by FastAudioSocket. Only a cancellation-installed
// write deadline is cleared, after its callback finishes and before unlocking.
func (s *FastAudioSocket) playbackContext(playerCtx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(playerCtx)
	stopCall := context.AfterFunc(s.callCtx, cancel)
	done := make(chan struct{})
	stopWrite := context.AfterFunc(ctx, func() {
		_ = s.conn.SetWriteDeadline(time.Now())
		close(done)
	})
	return ctx, func() {
		stopCall()
		if !stopWrite() {
			<-done
			_ = s.conn.SetWriteDeadline(time.Time{})
		}
		cancel()
	}
}
