package fastaudiosocket

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestReceivedPayloadsRemainOwnedByCaller(t *testing.T) {
	var wire []byte
	sizes := []int{160, 320, 1920, 65535, 320}
	for i, size := range sizes {
		wire = append(wire, frame(PacketTypeAudio, bytes.Repeat([]byte{byte(i + 1)}, size))...)
	}
	s := &FastAudioSocket{conn: &fragmentedConn{r: bytes.NewReader(wire), limit: len(wire)}}
	var packets []PacketReader
	for range sizes {
		p, err := s.readChunk()
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, p)
	}
	// Retain every packet across subsequent reads and buffer refills.
	for i, p := range packets {
		if int(p.Length) != sizes[i] || !bytes.Equal(p.Payload, bytes.Repeat([]byte{byte(i + 1)}, sizes[i])) {
			t.Fatalf("packet %d changed after later reads", i)
		}
	}
	packets[0].Payload[0] = 99
	if packets[1].Payload[0] != 2 {
		t.Fatal("independent packets share writable storage")
	}
}

func TestReceiveDoesNotWaitForAnotherFrame(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := bytes.Repeat([]byte{0xab}, 320)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = client.Write(frame(PacketTypeUUID, make([]byte, 16)))
		_, _ = client.Write(frame(PacketTypeAudio, payload))
		// Leave the connection open with no following frame.
		<-ctx.Done()
	}()
	s, err := NewFastAudioSocket(ctx, server, false, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case p, ok := <-s.AudioChan:
			if !ok {
				t.Fatal("closed before audio")
			}
			if p.SilenceSuppressed {
				continue
			}
			if p.Type != PacketTypeAudio || !bytes.Equal(p.Payload, payload) {
				t.Fatal("unexpected first frame")
			}
			cancel()
			<-writerDone
			return
		case <-deadline.C:
			t.Fatal("reader waited for more data after a complete frame")
		}
	}
}

type countedReadConn struct {
	net.Conn
	reads uint64
}

func (c *countedReadConn) Read(b []byte) (int, error) {
	c.reads++
	return c.Conn.Read(b)
}

// BenchmarkReceiveTCP includes real TCP reads. The producer writes complete
// frames either individually or in bursts; the library still returns one
// independently owned payload per frame. It does not model real-time pacing.
func BenchmarkReceiveTCP(b *testing.B) {
	for _, batch := range []int{1, 16} {
		b.Run(fmt.Sprintf("frames_per_write_%d", batch), func(b *testing.B) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			client, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				b.Fatal(err)
			}
			defer client.Close()
			server, err := l.Accept()
			if err != nil {
				b.Fatal(err)
			}
			defer server.Close()
			conn := &countedReadConn{Conn: server}
			s := &FastAudioSocket{conn: conn}
			wire := bytes.Repeat(frame(PacketTypeAudio, bytes.Repeat([]byte{0xab}, 320)), batch)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					if _, err := client.Write(wire); err != nil {
						return
					}
				}
			}()
			b.ReportAllocs()
			b.SetBytes(320)
			for b.Loop() {
				p, err := s.readChunk()
				if err != nil || len(p.Payload) != 320 || p.Payload[0] != 0xab {
					b.Fatalf("receive failed: %v", err)
				}
			}
			b.ReportMetric(float64(conn.reads)/float64(b.N), "reads/frame")
			client.Close()
			<-done
		})
	}
}
