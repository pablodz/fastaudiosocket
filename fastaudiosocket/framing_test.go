package fastaudiosocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

type fragmentedConn struct {
	net.Conn
	r     *bytes.Reader
	limit int
}

func (c *fragmentedConn) Read(b []byte) (int, error) { return c.r.Read(b[:min(len(b), c.limit)]) }
func frame(typ byte, payload []byte) []byte {
	b := make([]byte, HeaderSize+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint16(b[1:], uint16(len(payload)))
	copy(b[HeaderSize:], payload)
	return b
}
func TestReadFragmentedTCPFrames(t *testing.T) {
	for _, limit := range []int{1, 2, 7, 160, 323} {
		uuid := []byte{0, 0, 0, 0, 0, 0, 64, 0, 128, 0, 0, 0, 0, 0, 0, 1}
		audio := bytes.Repeat([]byte{0xab}, 320)
		wire := append(frame(PacketTypeUUID, uuid), frame(PacketTypeAudio, audio)...)
		wire = append(wire, frame(PacketTypeDTMF, []byte{'5'})...)
		wire = append(wire, frame(PacketTypeHangup, nil)...)
		s := &FastAudioSocket{conn: &fragmentedConn{r: bytes.NewReader(wire), limit: limit}}
		if _, err := s.readUUID(); err != nil {
			t.Fatal(limit, err)
		}
		for _, want := range []struct {
			typ byte
			b   []byte
		}{{PacketTypeAudio, audio}, {PacketTypeDTMF, []byte{'5'}}, {PacketTypeHangup, nil}} {
			p, err := s.readChunk()
			if err != nil || p.Type != want.typ || !bytes.Equal(p.Payload, want.b) {
				t.Fatalf("fragment=%d got=%+v err=%v", limit, p, err)
			}
		}
	}
}
func TestTruncatedTCPFrameFails(t *testing.T) {
	for _, wire := range [][]byte{{PacketTypeAudio, 1}, frame(PacketTypeAudio, []byte{1, 2})[:4]} {
		s := &FastAudioSocket{conn: &fragmentedConn{r: bytes.NewReader(wire), limit: 1}}
		if _, err := s.readChunk(); err != io.ErrUnexpectedEOF {
			t.Fatal(err)
		}
	}
}
func BenchmarkReadFrame(b *testing.B) {
	wire := frame(PacketTypeAudio, make([]byte, 320))
	r := bytes.NewReader(wire)
	s := &FastAudioSocket{conn: &fragmentedConn{r: r, limit: 323}}
	b.ReportAllocs()
	b.SetBytes(320)
	for b.Loop() {
		r.Reset(wire)
		if _, err := s.readChunk(); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkPacerOverdue(b *testing.B) {
	p := newPacer(TickerInterval)
	p.start = p.start.Add(-TickerInterval)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if err := p.wait(ctx, ctx); err != nil {
			b.Fatal(err)
		}
	}
	p.stop()
}

func TestSocketCancellationWithBackpressure(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = client.Write(frame(PacketTypeUUID, make([]byte, 16)))
		for range 100 {
			if _, err := client.Write(frame(PacketTypeAudio, make([]byte, 320))); err != nil {
				return
			}
		}
	}()
	s, err := NewFastAudioSocket(ctx, server, false, true)
	if err != nil {
		t.Fatal(err)
	}
	// Fill both public queues while the application is not consuming audio.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("blocked socket reader was not released")
	}
	done := make(chan struct{})
	go func() {
		for range s.AudioChan {
		}
		for range s.MonitorChan {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("output channels were not closed")
	}
}
func TestHandshakeCancellation(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := NewFastAudioSocket(ctx, server, false, false); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("incomplete handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake ignored cancellation")
	}
}
func TestRemoteHangupDeliveredAndClosed(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go func() {
		_, _ = client.Write(frame(PacketTypeUUID, make([]byte, 16)))
		_, _ = client.Write(frame(PacketTypeHangup, nil))
	}()
	s, err := NewFastAudioSocket(context.Background(), server, false, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-s.AudioChan:
		if p.Type != PacketTypeHangup {
			t.Fatal(p)
		}
	case <-time.After(time.Second):
		t.Fatal("hangup not delivered")
	}
	select {
	case _, ok := <-s.AudioChan:
		if ok {
			t.Fatal("audio after hangup")
		}
	case <-time.After(time.Second):
		t.Fatal("audio channel not closed")
	}
}
