package fastaudiosocket

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

type testPlaybackControl struct {
	mu      sync.Mutex
	paused  bool
	changed chan struct{}
	played  time.Duration
}

func newTestPlaybackControl() *testPlaybackControl {
	return &testPlaybackControl{changed: make(chan struct{})}
}

func (c *testPlaybackControl) Wait(ctx context.Context) error {
	for {
		c.mu.Lock()
		paused := c.paused
		changed := c.changed
		c.mu.Unlock()
		if !paused {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (c *testPlaybackControl) Played(duration time.Duration) {
	c.mu.Lock()
	c.played += duration
	c.mu.Unlock()
}

func (c *testPlaybackControl) setPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

func TestControlledPlaybackPausesBetweenFrames(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := &FastAudioSocket{conn: server, callCtx: callCtx}
	control := newTestPlaybackControl()
	control.setPaused(true)

	done := make(chan error, 1)
	go func() {
		done <- socket.PlayControlled(context.Background(), make([]byte, 2*WriteChunkSize), control)
	}()

	client.SetReadDeadline(time.Now().Add(40 * time.Millisecond))
	buffer := make([]byte, MaxPacketSize)
	if _, err := client.Read(buffer); err == nil {
		t.Fatal("audio was written while playback was paused")
	}

	client.SetReadDeadline(time.Time{})
	control.setPaused(false)
	for range 2 {
		if _, err := client.Read(buffer); err != nil {
			t.Fatalf("failed to read resumed audio: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("controlled playback failed: %v", err)
	}
	if control.played != 2*TickerInterval {
		t.Fatalf("unexpected played duration: %v", control.played)
	}
}
