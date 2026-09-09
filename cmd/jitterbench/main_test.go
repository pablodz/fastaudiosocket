//go:build linux

package main

import (
	"testing"
	"time"
)

func TestReceiverModelDistinguishesArrivalJitterFromUnderrun(t *testing.T) {
	audio := pcm(10)
	start := time.Unix(0, 0)
	makeTrace := func(times []time.Duration) []arrival {
		var a []arrival
		for i, at := range times {
			a = append(a, arrival{At: start.Add(at), Bytes: 320, Payload: audio[i*320 : (i+1)*320]})
		}
		return a
	}
	// Both traces have a 60 ms interarrival gap. Only the second had 60 ms
	// of audio available before that gap; it must have no playout underrun.
	noReserve := analyze(makeTrace([]time.Duration{0, 60 * time.Millisecond, 60 * time.Millisecond}), false)
	reserved := analyze(makeTrace([]time.Duration{0, 0, 0, 60 * time.Millisecond}), false)
	if noReserve.GapMax != 60 || reserved.GapMax != 60 || noReserve.FIFOGap != 40 || reserved.FIFOGap != 0 {
		t.Fatal(noReserve, reserved)
	}
	if noReserve.PayloadErrors != 0 || reserved.PayloadErrors != 0 {
		t.Fatal("valid audio rejected")
	}
}
func TestAnalysisDetectsPayloadDamage(t *testing.T) {
	a := []arrival{{At: time.Unix(0, 0), Bytes: 320, Payload: make([]byte, 320)}}
	if analyze(a, false).PayloadErrors != 1 {
		t.Fatal("corruption went undetected")
	}
}
func TestULawExpansion(t *testing.T) {
	for _, v := range []struct {
		b    byte
		want int
	}{{0xff, 0}, {0x7f, 0}, {0x80, 32124}, {0x00, -32124}} {
		if got := decodeULaw(v.b); got != v.want {
			t.Fatalf("%x decoded to %d", v.b, got)
		}
	}
}

func TestRTPClockJumpIsNotHiddenByFIFOModel(t *testing.T) {
	start := time.Unix(0, 0)
	trace := []arrival{
		{At: start, Timestamp: 1000, Bytes: 160, Payload: make([]byte, 160)},
		{At: start.Add(20 * time.Millisecond), Timestamp: 1960, Bytes: 160, Payload: make([]byte, 160)},
	}
	r := analyze(trace, true)
	// Both packets arrived on time, but 800 samples (100 ms) are absent from
	// the RTP media clock. A FIFO-only metric must not hide that distinction.
	if r.FIFOGap != 0 || r.TimestampJumps != 1 || r.RTPClockGap != 100 {
		t.Fatal(r)
	}
}
