package fastaudiosocket

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

type recordingConn struct {
	net.Conn
	times []time.Time
	err   error
	short bool
}

func (c *recordingConn) SetWriteDeadline(time.Time) error { return nil }
func (c *recordingConn) Close() error                     { return nil }

func (c *recordingConn) Write(b []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.short {
		return len(b) - 1, nil
	}
	c.times = append(c.times, time.Now())
	return len(b), nil
}
func socketWithLead(t *testing.T, lead time.Duration) (*FastAudioSocket, *recordingConn) {
	t.Helper()
	c := &recordingConn{}
	s := &FastAudioSocket{conn: c, callCtx: context.Background()}
	if err := s.SetPlaybackOptions(PlaybackOptions{Lead: lead}); err != nil {
		t.Fatal(err)
	}
	return s, c
}
func TestLeadPrefillAndCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := socketWithLead(t, 100*time.Millisecond)
		control := newTestPlaybackControl()
		start := time.Now()
		if err := s.PlayControlled(context.Background(), make([]byte, 8*WriteChunkSize), control); err != nil {
			t.Fatal(err)
		}
		for i, at := range c.times {
			want := time.Duration(max(0, i-4)) * TickerInterval
			if at.Sub(start) != want {
				t.Fatalf("frame %d at %s, want %s", i, at.Sub(start), want)
			}
		}
		if control.played != 160*time.Millisecond || time.Since(start) != 60*time.Millisecond {
			t.Fatal("Played must mean successfully sent audio")
		}
		if s.PlaybackStats().EstimatedBuffered != 100*time.Millisecond {
			t.Fatal(s.PlaybackStats())
		}
		time.Sleep(100 * time.Millisecond)
		if s.PlaybackStats().EstimatedBuffered != 0 {
			t.Fatal("queue did not drain")
		}
	})
}
func TestLeadPreservesTimelineAcrossCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := socketWithLead(t, 100*time.Millisecond)
		start := time.Now()
		for range 12 {
			if err := s.Play(context.Background(), make([]byte, WriteChunkSize)); err != nil {
				t.Fatal(err)
			}
		}
		if c.times[11].Sub(start) != 140*time.Millisecond {
			t.Fatal("sequential calls accumulated lead", c.times)
		}
		if s.PlaybackStats().EstimatedBuffered != 100*time.Millisecond {
			t.Fatal(s.PlaybackStats())
		}
	})
}
func TestLeadStartsWhenStreamingAudioArrives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := socketWithLead(t, 100*time.Millisecond)
		ch := make(chan []byte)
		done := make(chan error, 1)
		go func() { done <- s.PlayStreaming(context.Background(), ch, nil) }()
		time.Sleep(time.Second)
		start := time.Now()
		ch <- make([]byte, 6*WriteChunkSize)
		close(ch)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if c.times[5].Sub(start) != 20*time.Millisecond {
			t.Fatal(c.times)
		}
		if s.PlaybackStats().Resyncs != 0 {
			t.Fatal("source startup counted as resync")
		}
	})
}

type stallingControl struct {
	frames int
	at     int
	stall  time.Duration
}

func (c *stallingControl) Wait(context.Context) error {
	if c.frames == c.at {
		time.Sleep(c.stall)
	}
	return nil
}
func (c *stallingControl) Played(time.Duration) { c.frames++ }
func TestLeadStallsAndBoundedCatchup(t *testing.T) {
	for _, stall := range []time.Duration{60 * time.Millisecond, 180 * time.Millisecond, time.Second} {
		t.Run(stall.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, c := socketWithLead(t, 100*time.Millisecond)
				start := time.Now()
				ctrl := &stallingControl{at: 15, stall: stall}
				if err := s.PlayControlled(context.Background(), make([]byte, 50*WriteChunkSize), ctrl); err != nil {
					t.Fatal(err)
				}
				if stall < 80*time.Millisecond && time.Since(start) != 900*time.Millisecond {
					t.Fatal("covered stall changed timeline", time.Since(start))
				}
				for i, at := range c.times {
					j := i
					for j > 0 && c.times[j-1].Equal(at) {
						j--
					}
					if i-j+1 > 5 {
						t.Fatal("burst exceeds lead")
					}
				}
				if s.PlaybackStats().EstimatedBuffered > 100*time.Millisecond {
					t.Fatal(s.PlaybackStats())
				}
				if stall == time.Second && s.PlaybackStats().Resyncs != 1 {
					t.Fatal("long stall must resync")
				}
			})
		})
	}
}
func TestLeadResyncMeasuredAgainstMediaDeadline(t *testing.T) {
	p := newPacer(TickerInterval)
	p.lead = 100 * time.Millisecond
	now := p.start.Add(MaxCatchUpDrift)
	if p.resync(now) {
		t.Fatal("dispatch lateness within lead must not trigger resync")
	}
	if !p.resync(now.Add(time.Nanosecond)) {
		t.Fatal("media drift above cap must resync")
	}
}
func TestLeadCancellationStopsPrefill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := socketWithLead(t, 100*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Play(ctx, make([]byte, 50*WriteChunkSize)) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(c.times) != 5 {
			t.Fatal("cancellation wrote extra frames", len(c.times))
		}
	})
}
func TestWriteFailuresAreReturnedAndNotCounted(t *testing.T) {
	for _, short := range []bool{false, true} {
		s, c := socketWithLead(t, 0)
		c.short = short
		want := io.ErrShortWrite
		if !short {
			want = io.ErrClosedPipe
			c.err = want
		}
		ctrl := newTestPlaybackControl()
		if err := s.PlayControlled(context.Background(), make([]byte, WriteChunkSize), ctrl); !errors.Is(err, want) {
			t.Fatal(err)
		}
		if ctrl.played != 0 || s.PlaybackStats().PacketsSent != 0 || s.PlaybackStats().WriteErrors != 1 {
			t.Fatal("failed write counted as playback")
		}
	}
}
func TestLeadOptionsValidation(t *testing.T) {
	s, _ := socketWithLead(t, 0)
	for _, lead := range []time.Duration{-20 * time.Millisecond, 21 * time.Millisecond} {
		if s.SetPlaybackOptions(PlaybackOptions{Lead: lead}) == nil {
			t.Fatal("accepted invalid lead")
		}
	}
}
func TestConcurrentPlaybackRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := socketWithLead(t, 100*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		ch := make(chan []byte)
		done := make(chan error, 1)
		go func() { done <- s.PlayStreaming(ctx, ch, nil) }()
		synctest.Wait()
		if err := s.Play(ctx, make([]byte, WriteChunkSize)); !errors.Is(err, ErrPlaybackInProgress) {
			t.Fatal(err)
		}
		cancel()
		<-done
	})
}

func TestCancellationInterruptsBlockedWrite(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	s := &FastAudioSocket{conn: server, callCtx: context.Background()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Play(ctx, make([]byte, 320)) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write ignored cancellation")
	}
	// The expired cancellation deadline must not poison the next playback.
	go func() { _, _ = io.CopyN(io.Discard, client, MaxPacketSize) }()
	if err := s.Play(context.Background(), make([]byte, 320)); err != nil {
		t.Fatal("next playback failed", err)
	}
}
func TestCallCancellationInterruptsPausedControl(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := socketWithLead(t, 100*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		s.callCtx = ctx
		c := newTestPlaybackControl()
		c.setPaused(true)
		done := make(chan error, 1)
		go func() { done <- s.PlayControlled(context.Background(), make([]byte, 320), c) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}
func TestLongStallRemainsObservableAfterResync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := socketWithLead(t, 100*time.Millisecond)
		ctrl := &stallingControl{at: 15, stall: time.Second}
		if err := s.PlayControlled(context.Background(), make([]byte, 30*320), ctrl); err != nil {
			t.Fatal(err)
		}
		stats := s.PlaybackStats()
		if stats.MaxScheduleLateness < 900*time.Millisecond || stats.Resyncs != 1 {
			t.Fatal("resync erased drift", stats)
		}
	})
}

func TestPartialWriteCancellationClosesDamagedStream(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	s := &FastAudioSocket{conn: server, callCtx: context.Background()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Play(ctx, make([]byte, 320)) }()
	// Consume only the header and one payload byte, then cancel mid-frame.
	if _, err := io.ReadFull(client, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("partial write remained blocked")
	}
	if _, err := client.Read(make([]byte, 320)); err != io.EOF {
		t.Fatal("damaged stream was left open", err)
	}
	if s.PlaybackStats().PacketsSent != 0 {
		t.Fatal("partial frame counted as sent")
	}
}
