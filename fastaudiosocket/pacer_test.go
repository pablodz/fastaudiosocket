package fastaudiosocket

import (
	"context"
	"testing"
	"time"
)

func TestPacerSendsFirstPacketWithoutWaiting(t *testing.T) {
	pace := newPacer(TickerInterval)

	started := time.Now()
	if err := pace.wait(context.Background(), context.Background()); err != nil {
		t.Fatalf("wait failed: %v", err)
	}

	if elapsed := time.Since(started); elapsed > 5*time.Millisecond {
		t.Fatalf("first packet waited %v; it must go out immediately", elapsed)
	}
}

func TestPacerCatchesUpAfterAStall(t *testing.T) {
	pace := newPacer(TickerInterval)
	pace.start = time.Now().Add(-100 * time.Millisecond)

	started := time.Now()
	for range 5 {
		if err := pace.wait(context.Background(), context.Background()); err != nil {
			t.Fatalf("wait failed: %v", err)
		}
		pace.advance()
	}

	if elapsed := time.Since(started); elapsed > 10*time.Millisecond {
		t.Fatalf("overdue packets took %v; they must go out back to back", elapsed)
	}
	if pace.sent != 5 {
		t.Fatalf("expected 5 packets sent, got %d", pace.sent)
	}
}

func TestPacerKeepsCadenceWhenOnTime(t *testing.T) {
	pace := newPacer(TickerInterval)

	started := time.Now()
	for range 3 {
		if err := pace.wait(context.Background(), context.Background()); err != nil {
			t.Fatalf("wait failed: %v", err)
		}
		pace.advance()
	}

	elapsed := time.Since(started)
	if elapsed < 2*TickerInterval-2*time.Millisecond {
		t.Fatalf("three packets took %v; the cadence collapsed", elapsed)
	}
	if elapsed > 2*TickerInterval+15*time.Millisecond {
		t.Fatalf("three packets took %v; the cadence drifted", elapsed)
	}
}

func TestPacerResyncsBeyondMaxDrift(t *testing.T) {
	pace := newPacer(TickerInterval)
	pace.sent = 10
	pace.start = time.Now().Add(-10*TickerInterval - 5*time.Second)

	if !pace.resync(time.Now()) {
		t.Fatal("a five second stall must resync instead of bursting")
	}

	if delay := time.Until(pace.due()); delay > time.Millisecond || delay < -time.Millisecond {
		t.Fatalf("after resync the next packet is due in %v; expected now", delay)
	}
}

func TestPacerDoesNotResyncWithinMaxDrift(t *testing.T) {
	pace := newPacer(TickerInterval)
	pace.start = time.Now().Add(-MaxCatchUpDrift / 2)

	if pace.resync(time.Now()) {
		t.Fatal("a drift under the cap must be caught up, not resynced away")
	}
}

func TestPacerHonoursCancellation(t *testing.T) {
	pace := newPacer(TickerInterval)
	pace.advance()

	playerCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := pace.wait(playerCtx, context.Background()); err == nil {
		t.Fatal("a cancelled playback context must stop the writer")
	}
}
