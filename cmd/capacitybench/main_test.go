//go:build linux

package main

import (
	"encoding/binary"
	"testing"
	"time"
)

func encodedFrame(audio []byte, index int) []byte {
	b := make([]byte, 160)
	for i := range b {
		b[i] = encodeULaw(int16(binary.LittleEndian.Uint16(audio[index*320+i*2:])))
	}
	return b
}

func TestKernelTimestampValidation(t *testing.T) {
	for _, size := range []int{8, 16} {
		b := make([]byte, size)
		if size == 16 {
			binary.NativeEndian.PutUint64(b, 123)
			binary.NativeEndian.PutUint64(b[8:], 456)
		} else {
			binary.NativeEndian.PutUint32(b, 123)
			binary.NativeEndian.PutUint32(b[4:], 456)
		}
		at, ok := kernelTime(b)
		if !ok || !at.Equal(time.Unix(123, 456)) {
			t.Fatalf("size=%d: %v %v", size, at, ok)
		}
	}
	if _, ok := kernelTime([]byte{1}); ok {
		t.Fatal("accepted truncated timestamp")
	}
	b := make([]byte, 16)
	binary.NativeEndian.PutUint64(b[8:], 1e9)
	if _, ok := kernelTime(b); ok {
		t.Fatal("accepted invalid nanoseconds")
	}
}

func TestRTPSequenceWrapAndLoss(t *testing.T) {
	audio := tone(4)
	at := time.Unix(1000, 0)
	var m callMetrics
	m.consume(at, encodedFrame(audio, 0), 65535, 0, true, audio)
	m.consume(at.Add(20*time.Millisecond), encodedFrame(audio, 1), 0, 160, true, audio)
	m.consume(at.Add(60*time.Millisecond), encodedFrame(audio, 3), 2, 480, true, audio)
	if m.SequenceMissing != 1 || m.PayloadErrors != 0 || m.TimestampJumps != 0 {
		t.Fatalf("loss must not cascade into false corruption/clock errors: %+v", m)
	}
	if m.FIFOGapMS != 20 {
		t.Fatalf("lost audio gap = %v", m.FIFOGapMS)
	}
}

func TestClockDiscontinuityIsIndependentOfFIFO(t *testing.T) {
	audio := tone(2)
	at := time.Unix(1000, 0)
	var m callMetrics
	m.consume(at, encodedFrame(audio, 0), 1, 0, true, audio)
	m.consume(at.Add(20*time.Millisecond), encodedFrame(audio, 1), 2, 960, true, audio)
	if m.FIFOGapMS != 0 || m.ClockGapMS != 100 || m.TimestampJumps != 1 {
		t.Fatalf("clock gap hidden by FIFO model: %+v", m)
	}
}

func TestPayloadCorruptionAndBurstReserve(t *testing.T) {
	audio := tone(4)
	at := time.Unix(1000, 0)
	var m callMetrics
	for i := 0; i < 3; i++ {
		m.consume(at, audio[i*320:(i+1)*320], 0, 0, false, audio)
	}
	bad := append([]byte(nil), audio[960:]...)
	bad[0] ^= 1
	m.consume(at.Add(90*time.Millisecond), bad, 0, 0, false, audio)
	if m.PayloadErrors != 1 || m.FIFOGapMS != 30 || m.PeakFIFOms != 60 {
		t.Fatalf("unexpected metrics: %+v", m)
	}
}

func TestProcCPUAllowsSpacesInCommand(t *testing.T) {
	if got := statCPU("123 (command with spaces) S 0 0 0 0 0 0 0 0 0 0 150 50", 100); got != 2000 {
		t.Fatalf("CPU ms = %v", got)
	}
	if got := statCPU("123 (truncated) S", 100); got != 0 {
		t.Fatal(got)
	}
}

func TestSummaryDoesNotHideOneFailedCall(t *testing.T) {
	c := []controllerCall{{metrics: callMetrics{Packets: 10}}, {metrics: callMetrics{Packets: 9, SequenceMissing: 1, FIFOGapMS: 20}}}
	s := senderStats{Calls: []senderCall{{Sent: 10, Received: 10}, {Sent: 10, Received: 9}}}
	r := summarize(c, s, 10)
	if r["calls_wrong_count"] != 1 || r["fifo_gap_max_ms"] != float64(20) || r["fifo_gap_mean_ms"] != float64(10) || r["sequence_missing"] != 1 {
		t.Fatal(r)
	}
}
